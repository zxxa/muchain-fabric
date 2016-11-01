/*
Copyright IBM Corp. 2016 All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

		 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ledger

import (
	"bytes"
	"fmt"
	"reflect"
	"sync"

	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric/core/db"
	"github.com/hyperledger/fabric/core/ledger/state"
	"github.com/hyperledger/fabric/core/ledger/state/chaincodest"
	chstatemgmt "github.com/hyperledger/fabric/core/ledger/state/chaincodest/statemgmt"
	txsetstmgmt "github.com/hyperledger/fabric/core/ledger/state/txsetst/statemgmt"
	"github.com/hyperledger/fabric/events/producer"
	"github.com/op/go-logging"
	"github.com/tecbot/gorocksdb"

	"github.com/hyperledger/fabric/core/ledger/state/txsetst"
	"github.com/hyperledger/fabric/protos"
	"golang.org/x/net/context"
	"errors"
	"github.com/hyperledger/fabric/core/container"
	"github.com/hyperledger/fabric/core/crypto/txset"
)

var ledgerLogger = logging.MustGetLogger("ledger")

//ErrorType represents the type of a ledger error
type ErrorType string

const (
	//ErrorTypeInvalidArgument used to indicate the invalid input to ledger method
	ErrorTypeInvalidArgument = ErrorType("InvalidArgument")
	//ErrorTypeOutOfBounds used to indicate that a request is out of bounds
	ErrorTypeOutOfBounds = ErrorType("OutOfBounds")
	//ErrorTypeResourceNotFound used to indicate if a resource is not found
	ErrorTypeResourceNotFound = ErrorType("ResourceNotFound")
	//ErrorTypeBlockNotFound used to indicate if a block is not found when looked up by it's hash
	ErrorTypeBlockNotFound = ErrorType("ErrorTypeBlockNotFound")
)

//Error can be used for throwing an error from ledger code.
type Error struct {
	errType ErrorType
	msg     string
}

func (ledgerError *Error) Error() string {
	return fmt.Sprintf("LedgerError - %s: %s", ledgerError.errType, ledgerError.msg)
}

//Type returns the type of the error
func (ledgerError *Error) Type() ErrorType {
	return ledgerError.errType
}

func newLedgerError(errType ErrorType, msg string) *Error {
	return &Error{errType, msg}
}

var (
	// ErrOutOfBounds is returned if a request is out of bounds
	ErrOutOfBounds = newLedgerError(ErrorTypeOutOfBounds, "ledger: out of bounds")

	// ErrResourceNotFound is returned if a resource is not found
	ErrResourceNotFound = newLedgerError(ErrorTypeResourceNotFound, "ledger: resource not found")
)

// Ledger - the struct for openchain ledger
type Ledger struct {
	blockchain     *blockchain
	chaincodeState *chaincodest.State
	txSetState     *txsetst.TxSetState
	currentID      interface{}
}

var ledger *Ledger
var ledgerError error
var once sync.Once

// GetLedger - gives a reference to a 'singleton' ledger
func GetLedger() (*Ledger, error) {
	once.Do(func() {
		ledger, ledgerError = GetNewLedger()
	})
	return ledger, ledgerError
}

// GetNewLedger - gives a reference to a new ledger TODO need better approach
func GetNewLedger() (*Ledger, error) {
	blockchain, err := newBlockchain()
	if err != nil {
		return nil, err
	}

	chaincodeState := chaincodest.NewState()
	txSetState := txsetst.NewTxSetState()
	return &Ledger{blockchain, chaincodeState, txSetState, nil}, nil
}

/////////////////// Transaction-batch related methods ///////////////////////////////
/////////////////////////////////////////////////////////////////////////////////////

// BeginTxBatch - gets invoked when next round of transaction-batch execution begins
func (ledger *Ledger) BeginTxBatch(id interface{}) error {
	err := ledger.checkValidIDBegin()
	if err != nil {
		return err
	}
	ledger.currentID = id
	return nil
}

// GetTXBatchPreviewBlockInfo returns a preview block info that will
// contain the same information as GetBlockchainInfo will return after
// ledger.CommitTxBatch is called with the same parameters. If the
// state is modified by a transaction between these two calls, the
// contained hash will be different.
func (ledger *Ledger) GetTXBatchPreviewBlockInfo(id interface{},
	transactions []*protos.InBlockTransaction, metadata []byte) (*protos.BlockchainInfo, error) {
	err := ledger.checkValidIDCommitORRollback(id)
	if err != nil {
		return nil, err
	}
	chaincodeStHash, err := ledger.chaincodeState.GetHash()
	if err != nil {
		return nil, err
	}
	txSetStHash, err := ledger.txSetState.GetHash()
	if err != nil {
		return nil, err
	}
	block := ledger.blockchain.buildBlock(protos.NewBlock(transactions, metadata), chaincodeStHash, txSetStHash)
	info := ledger.blockchain.getBlockchainInfoForBlock(ledger.blockchain.getSize()+1, block)
	return info, nil
}

// CommitTxBatch - gets invoked when the current transaction-batch needs to be committed
// This function returns successfully iff the transactions details and state changes (that
// may have happened during execution of this transaction-batch) have been committed to permanent storage
func (ledger *Ledger) CommitTxBatch(id interface{}, transactions []*protos.InBlockTransaction, transactionResults []*protos.TransactionResult, metadata []byte) error {
	err := ledger.checkValidIDCommitORRollback(id)
	if err != nil {
		return err
	}

	chaincodeStHash, err := ledger.chaincodeState.GetHash()
	if err != nil {
		ledger.resetForNextTxGroup(false)
		ledger.blockchain.blockPersistenceStatus(false)
		return err
	}

	txSetStHash, err := ledger.txSetState.GetHash()
	if err != nil {
		ledger.resetForNextTxGroup(false)
		ledger.blockchain.blockPersistenceStatus(false)
	}

	writeBatch := gorocksdb.NewWriteBatch()
	defer writeBatch.Destroy()
	block := protos.NewBlock(transactions, metadata)

	ccEvents := []*protos.ChaincodeEvent{}

	var numErroneusTxs = 0
	if transactionResults != nil {
		ccEvents = make([]*protos.ChaincodeEvent, len(transactionResults))
		for i := 0; i < len(transactionResults); i++ {
			if transactionResults[i].ChaincodeEvent != nil {
				ccEvents[i] = transactionResults[i].ChaincodeEvent
			} else {
				//We need the index so we can map the chaincode
				//event to the transaction that generated it.
				//Hence need an entry for cc event even if one
				//wasn't generated for the transaction. We cannot
				//use a nil cc event as protobuf does not like
				//elements of a repeated array to be nil.
				//
				//We should discard empty events without chaincode
				//ID when sending out events.
				ccEvents[i] = &protos.ChaincodeEvent{}
			}
			if transactionResults[i].ErrorCode != 0 {
				ledgerLogger.Infof("Transaction with id %s contained errors: %s", transactionResults[i].Txid, transactionResults[i].Error)
				numErroneusTxs++
			}
		}
	}

	//store chaincode events directly in NonHashData. This will likely change in New Consensus where we can move them to Transaction
	block.NonHashData = &protos.NonHashData{ChaincodeEvents: ccEvents}
	newBlockNumber, err := ledger.blockchain.addPersistenceChangesForNewBlock(context.TODO(), block, chaincodeStHash, txSetStHash, writeBatch)
	if err != nil {
		ledger.resetForNextTxGroup(false)
		ledger.blockchain.blockPersistenceStatus(false)
		return err
	}
	ledger.chaincodeState.AddChangesForPersistence(newBlockNumber, writeBatch)
	ledger.txSetState.AddChangesForPersistence(newBlockNumber, writeBatch)
	opt := gorocksdb.NewDefaultWriteOptions()
	defer opt.Destroy()
	dbErr := db.GetDBHandle().DB.Write(opt, writeBatch)
	if dbErr != nil {
		ledger.resetForNextTxGroup(false)
		ledger.blockchain.blockPersistenceStatus(false)
		return dbErr
	}

	ledger.resetForNextTxGroup(true)
	ledger.blockchain.blockPersistenceStatus(true)

	sendProducerBlockEvent(block)

	//send chaincode events from transaction results
	sendChaincodeEvents(transactionResults)

	if numErroneusTxs != 0 {
		ledgerLogger.Debug("There were some erroneous transactions. We need to send a 'TX rejected' message here.")
	}
	return nil
}

// CommitResetTxBatch - gets invoked when the current transaction-batch needs to be committed
// This function returns successfully iff the transactions details and state changes (that
// may have happened during execution of this transaction-batch) have been committed to permanent storage
func (ledger *Ledger) CommitResetTxBatch() error {
	if !ledger.blockchain.isResetting {
		return fmt.Errorf("Cannot commit a reset tx batch bacause the blockchain is not in a reset status.")
	}

	writeBatch := gorocksdb.NewWriteBatch()
	defer writeBatch.Destroy()
	ledger.chaincodeState.AddChangesForPersistence(ledger.GetCurrentBlockEx(), writeBatch)
	opt := gorocksdb.NewDefaultWriteOptions()
	defer opt.Destroy()
	dbErr := db.GetDBHandle().DB.Write(opt, writeBatch)
	if dbErr != nil {
		ledger.resetForNextTxGroup(false)
		ledger.blockchain.blockPersistenceStatus(false)
		return dbErr
	}

	return ledger.blockchain.advanceResetBlock()
}


// RollbackTxBatch - Discards all the state changes that may have taken place during the execution of
// current transaction-batch
func (ledger *Ledger) RollbackTxBatch(id interface{}) error {
	ledgerLogger.Debugf("RollbackTxBatch for id = [%s]", id)
	err := ledger.checkValidIDCommitORRollback(id)
	if err != nil {
		return err
	}
	ledger.resetForNextTxGroup(false)
	return nil
}

// ChainTxBegin - Marks the begin of a new transaction in the ongoing batch
func (ledger *Ledger) ChainTxBegin(txID string) {
	ledger.chaincodeState.TxBegin(txID)
}

// SetTxBegin - Marks the begin of a new tx set transaction in the ongoing batch
func (ledger *Ledger) SetTxBegin(txID string) {
	ledger.txSetState.TxBegin(txID)
}

// ChainTxFinished - Marks the finish of the on-going transaction.
// If txSuccessful is false, the state changes made by the transaction are discarded
func (ledger *Ledger) ChainTxFinished(txID string, txSuccessful bool) {
	ledger.chaincodeState.TxFinish(txID, txSuccessful)
}

// SetTxFinished - Marks the finish of the on-going tx set transaction.
// If txSuccessful is false, the state changes made by the transaction are discarded
func (ledger *Ledger) SetTxFinished(txID string, txSuccessful bool) {
	ledger.txSetState.TxFinish(txID, txSuccessful)
}

/////////////////// world-state related methods /////////////////////////////////////
/////////////////////////////////////////////////////////////////////////////////////

// GetTempStateHash - Computes state hash by taking into account the state changes that may have taken
// place during the execution of current transaction-batch
func (ledger *Ledger) GetTempStateHash() ([]byte, error) {
	return ledger.chaincodeState.GetHash()
}

// GetTemptTxSetStateHash - Computes the transaction set state hash by taking into account the state changes that may
// have taken place during the execution of current transaction-batch
func (ledger *Ledger) GetTempTxSetStateHash() ([]byte, error) {
	return ledger.txSetState.GetHash()
}

// GetTempStateHashWithTxDeltaStateHashes - In addition to the state hash (as defined in method GetTempStateHash),
// this method returns a map [txUuid of Tx --> cryptoHash(stateChangesMadeByTx)]
// Only successful txs appear in this map
func (ledger *Ledger) GetTempStateHashWithTxDeltaStateHashes() ([]byte, map[string][]byte, error) {
	chaincodeStHash, err := ledger.chaincodeState.GetHash()
	return chaincodeStHash, ledger.chaincodeState.GetTxStateDeltaHash(), err
}

// GetTempTxSetStateHashWithTxDeltaStateHashes - In addition to the transactions set state hash
// (as defined in method GetTempStateHash), this method returns a map [txUuid of Tx --> cryptoHash(stateChangesMadeByTx)]
// Only successful txs appear in this map
func (ledger *Ledger) GetTempTxSetStateHashWithTxDeltaStateHashes() ([]byte, map[string][]byte, error) {
	txSetStateHash, err := ledger.txSetState.GetHash()
	return txSetStateHash, ledger.txSetState.GetTxStateDeltaHash(), err
}

// GetState get state for chaincodeID and key. If committed is false, this first looks in memory
// and if missing, pulls from db.  If committed is true, this pulls from the db only.
func (ledger *Ledger) GetState(chaincodeID string, key string, committed bool) ([]byte, error) {
	return ledger.chaincodeState.Get(chaincodeID, key, committed)
}

// GetTxSetState get state for txSetID. If committed is false, this first looks in memory
// and if missing, pulls from db.  If committed is true, this pulls from the db only.
func (ledger *Ledger) GetTxSetState(txSetID string, committed bool) (*protos.TxSetStateValue, error) {
	return ledger.txSetState.Get(txSetID, committed)
}

// GetOlderTBModBlock - returns the older block to be modified by a mutant transaction at the next commit
// if not block is to be modified it returns false in the second argument
func (ledger *Ledger) GetOlderTBModBlock() (uint64, bool) {
	return ledger.txSetState.GetOlderBlockMod()
}

// GetStateRangeScanIterator returns an iterator to get all the keys (and values) between startKey and endKey
// (assuming lexical order of the keys) for a chaincodeID.
// If committed is true, the key-values are retrieved only from the db. If committed is false, the results from db
// are mergerd with the results in memory (giving preference to in-memory data)
// The key-values in the returned iterator are not guaranteed to be in any specific order
func (ledger *Ledger) GetStateRangeScanIterator(chaincodeID string, startKey string, endKey string, committed bool) (stcomm.RangeScanIterator, error) {
	return ledger.chaincodeState.GetRangeScanIterator(chaincodeID, startKey, endKey, committed)
}

// SetState sets state to given value for chaincodeID and key. Does not immediately write to DB
func (ledger *Ledger) SetState(chaincodeID string, key string, value []byte) error {
	if key == "" || value == nil {
		return newLedgerError(ErrorTypeInvalidArgument,
			fmt.Sprintf("An empty string key or a nil value is not supported. Method invoked with key='%s', value='%#v'", key, value))
	}
	return ledger.chaincodeState.Set(chaincodeID, key, value)
}

// SetTxSetState sets state to given value for txSetID. Does not immediately write to DB
func (ledger *Ledger) SetTxSetState(txSetID string, txSetStateValue *protos.TxSetStateValue) error {
	if txSetStateValue == nil {
		return newLedgerError(ErrorTypeInvalidArgument,
			fmt.Sprintf("An empty transaction set state value is not supported. Method invoked with stateValue='%#v'", txSetStateValue))
	}
	previousValue, err := ledger.GetTxSetState(txSetID, true)
	if err != nil {
		return newLedgerError(ErrorTypeResourceNotFound,
			fmt.Sprintf("Error while trying to retrieve the previous state for txSetID='%s', err='%#v'", txSetID, err))
	}
	if previousValue == nil {
		previousValue = &protos.TxSetStateValue{}
	}
	if txSetStateValue.Nonce != previousValue.Nonce+1 {
		return newLedgerError(ErrorTypeInvalidArgument,
			fmt.Sprintf("Wrong nonce update. Previous nonce: %d, new nonce: %d", previousValue.Nonce, txSetStateValue.Nonce))
	}
	if previousValue.IntroBlock != 0 && previousValue.IntroBlock != txSetStateValue.IntroBlock {
		return newLedgerError(ErrorTypeInvalidArgument,
			fmt.Sprintf("A mutant transaction or an extension to a set cannot modify the intro block. Prev Intro Block: [%d], New Intro Block: [%d]", previousValue.IntroBlock, txSetStateValue.IntroBlock))
	}
	if previousValue.IntroBlock != 0 && previousValue.Index != txSetStateValue.Index {
		err = previousValue.IsValidMutation(txSetStateValue)
		if err != nil {
			return newLedgerError(ErrorTypeInvalidArgument, err.Error())
		}
	} else {
		err = previousValue.IsValidBlockExtension(txSetStateValue)
		if err != nil {
			return newLedgerError(ErrorTypeInvalidArgument, err.Error())
		}
	}
	return ledger.txSetState.Set(txSetID, txSetStateValue)
}

// ResetToBlock resets the chaincode state to the state at the end of the given block (i.e. beginning of the next),
// keeping the rest of the data intact
func (ledger *Ledger) ResetToBlock(blockNum uint64) error {
	stateAtBlock, err := ledger.chaincodeState.FetchBlockStateDeltaFromDB(blockNum)
	if err != nil {
		return fmt.Errorf("Unable to reset the state to block %d, the state at that block could not be retrieved.", blockNum, err)
	}
	err = ledger.chaincodeState.DeleteState()
	if err != nil {
		return fmt.Errorf("Unable to reset the state to block %d, the state could not be erased.", blockNum, err)
	}
	ledger.chaincodeState.ApplyStateDelta(stateAtBlock)
	err = ledger.chaincodeState.CommitStateDelta()
	if err != nil {
		return err
	}
	return ledger.blockchain.startResetFromBlock(blockNum + 1)
}

func (ledger *Ledger) ConcludeReset() error {
	return ledger.blockchain.endReset()
}

// DeleteState tracks the deletion of state for chaincodeID and key. Does not immediately writes to DB
func (ledger *Ledger) DeleteState(chaincodeID string, key string) error {
	return ledger.chaincodeState.Delete(chaincodeID, key)
}

// CopyState copies all the key-values from sourceChaincodeID to destChaincodeID
func (ledger *Ledger) CopyState(sourceChaincodeID string, destChaincodeID string) error {
	return ledger.chaincodeState.CopyState(sourceChaincodeID, destChaincodeID)
}

// GetStateMultipleKeys returns the values for the multiple keys.
// This method is mainly to amortize the cost of grpc communication between chaincode shim peer
func (ledger *Ledger) GetStateMultipleKeys(chaincodeID string, keys []string, committed bool) ([][]byte, error) {
	return ledger.chaincodeState.GetMultipleKeys(chaincodeID, keys, committed)
}

// SetStateMultipleKeys sets the values for the multiple keys.
// This method is mainly to amortize the cost of grpc communication between chaincode shim peer
func (ledger *Ledger) SetStateMultipleKeys(chaincodeID string, kvs map[string][]byte) error {
	return ledger.chaincodeState.SetMultipleKeys(chaincodeID, kvs)
}

// GetStateSnapshot returns a point-in-time view of the global state for the current block. This
// should be used when transferring the state from one peer to another peer. You must call
// stateSnapshot.Release() once you are done with the snapshot to free up resources.
func (ledger *Ledger) GetStateSnapshot() (*stcomm.StateSnapshot, *stcomm.StateSnapshot, error) {
	dbSnapshot := db.GetDBHandle().GetSnapshot()
	dbSnapshotForTxSet := db.GetDBHandle().GetSnapshot()
	blockHeight, err := fetchBlockchainSizeFromSnapshot(dbSnapshot)
	if err != nil {
		dbSnapshot.Release()
		return nil, nil, err
	}
	if 0 == blockHeight {
		dbSnapshot.Release()
		return nil, nil, errors.New("Blockchain has no blocks, cannot determine block number")
	}
	chainSnap, err := ledger.chaincodeState.GetSnapshot(blockHeight-1, dbSnapshot)
	if err != nil {
		return nil, nil, err
	}
	txSetSnap, err := ledger.txSetState.GetTxSetSnapshot(blockHeight-1, dbSnapshotForTxSet)
	return chainSnap, txSetSnap, nil
}

func (ledger *Ledger) GetDeltaFromGenesis(blockNum uint64) (*chstatemgmt.StateDelta, error) {
	return ledger.chaincodeState.FetchBlockStateDeltaFromDB(blockNum)
}

// GetStateDelta will return the state delta for the specified block if
// available.  If not available because it has been discarded, returns nil,nil.
func (ledger *Ledger) GetStateDelta(blockNumber uint64) (*chstatemgmt.StateDelta, *txsetstmgmt.TxSetStateDelta, error) {
	if blockNumber >= ledger.GetBlockchainSize() {
		return nil, nil, ErrOutOfBounds
	}
	chainstDelta, err := ledger.chaincodeState.FetchStateDeltaFromDB(blockNumber)
	if err != nil {
		return nil, nil, err
	}
	txSetStDelta, err := ledger.txSetState.FetchStateDeltaFromDB(blockNumber)
	if err != nil {
		return nil, nil, err
	}
	return chainstDelta, txSetStDelta, nil
}

// ApplyStateDelta applies a state delta to the current state. This is an
// in memory change only. You must call ledger.CommitStateDelta to persist
// the change to the DB.
// This should only be used as part of state synchronization. State deltas
// can be retrieved from another peer though the Ledger.GetStateDelta function
// or by creating state deltas with keys retrieved from
// Ledger.GetStateSnapshot(). For an example, see TestSetRawState in
// ledger_test.go
// Note that there is no order checking in this function and it is up to
// the caller to ensure that deltas are applied in the correct order.
// For example, if you are currently at block 8 and call this function
// with a delta retrieved from Ledger.GetStateDelta(10), you would now
// be in a bad state because you did not apply the delta for block 9.
// It's possible to roll the state forwards or backwards using
// stateDelta.RollBackwards. By default, a delta retrieved for block 3 can
// be used to roll forwards from state at block 2 to state at block 3. If
// stateDelta.RollBackwards=false, the delta retrieved for block 3 can be
// used to roll backwards from the state at block 3 to the state at block 2.
func (ledger *Ledger) ApplyStateDelta(id interface{}, chaincodeDelta *chstatemgmt.StateDelta, txSetStDelta *txsetstmgmt.TxSetStateDelta) error {
	err := ledger.checkValidIDBegin()
	if err != nil {
		return err
	}
	ledger.currentID = id
	ledger.chaincodeState.ApplyStateDelta(chaincodeDelta)
	ledger.txSetState.ApplyStateDelta(txSetStDelta)
	return nil
}

// CommitStateDelta will commit the state delta passed to ledger.ApplyStateDelta
// to the DB
func (ledger *Ledger) CommitStateDelta(id interface{}) error {
	err := ledger.checkValidIDCommitORRollback(id)
	if err != nil {
		return err
	}
	defer ledger.resetForNextTxGroup(true)
	err = ledger.chaincodeState.CommitStateDelta()
	if err != nil {
		return err
	}
	return ledger.txSetState.CommitStateDelta()
}

// RollbackStateDelta will discard the state delta passed
// to ledger.ApplyStateDelta
func (ledger *Ledger) RollbackStateDelta(id interface{}) error {
	err := ledger.checkValidIDCommitORRollback(id)
	if err != nil {
		return err
	}
	ledger.resetForNextTxGroup(false)
	return nil
}

// DeleteALLStateKeysAndValues deletes all keys and values from the state.
// This is generally only used during state synchronization when creating a
// new state from a snapshot.
func (ledger *Ledger) DeleteALLStateKeysAndValues() error {
	err := ledger.chaincodeState.DeleteState()
	if err != nil {
		return err
	}
	return ledger.txSetState.DeleteState()
}

/////////////////// blockchain related methods /////////////////////////////////////
/////////////////////////////////////////////////////////////////////////////////////

// GetBlockchainInfo returns information about the blockchain ledger such as
// height, current block hash, and previous block hash.
func (ledger *Ledger) GetBlockchainInfo() (*protos.BlockchainInfo, error) {
	return ledger.blockchain.getBlockchainInfo()
}

// GetBlockByNumber return block given the number of the block on blockchain.
// Lowest block on chain is block number zero
func (ledger *Ledger) GetBlockByNumber(blockNumber uint64) (*protos.Block, error) {
	if blockNumber >= ledger.GetBlockchainSize() {
		return nil, ErrOutOfBounds
	}
	return ledger.blockchain.getBlock(blockNumber)
}

// GetBlockchainSize returns number of blocks in blockchain
func (ledger *Ledger) GetBlockchainSize() uint64 {
	return ledger.blockchain.getSize()
}

// GetCurrentBlockEx returns the block number at which the transactions would be applied
func (ledger *Ledger) GetCurrentBlockEx() uint64 {
	if ledger.blockchain.isResetting {
		return ledger.blockchain.getSizeReset()
	} else {
		return ledger.blockchain.getSize()
	}
}

// IsResetting returns true if a reset is currently occurring
func (ledger *Ledger) IsResetting() bool {
	return ledger.blockchain.isResetting
}

func (ledger *Ledger) GetCurrentDefaultByID(txSetID string) (*protos.Transaction, error) {
	someInBlockWithMatchingID, err := ledger.blockchain.getTransactionByID(txSetID)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve queried txsetid default transaction. ID: [%s], Err: [%s]", txSetID, err)
	}
	return ledger.GetCurrentDefault(someInBlockWithMatchingID, false)
}

// GetCurrentDefault returns the current default transaction of the queried txSetID
func (ledger *Ledger) GetCurrentDefault(inBlockTx *protos.InBlockTransaction, committed bool) (*protos.Transaction, error) {
	txSetStValue, err := ledger.GetTxSetState(inBlockTx.Txid, committed)
	if err != nil {
		return nil, fmt.Errorf("Failed to retrieve the txSet state, txID: %s, err: %s.", inBlockTx.Txid, err)
	}
	var defTxBytes []byte
	if inBlockTx.GetTransactionSet() != nil {
		defTxBytes = inBlockTx.GetTransactionSet().Transactions[inBlockTx.GetTransactionSet().DefaultInx]
	}
	if txSetStValue == nil {
		if inBlockTx.GetTransactionSet() == nil || len(inBlockTx.GetTransactionSet().Transactions) == 0 {
			return nil, errors.New("The given transaction is not a transactions set.")
		}
		// Try to see if this was an encapsulated transaction
		recoveredTx := &protos.Transaction{}
		err := proto.Unmarshal(inBlockTx.GetTransactionSet().Transactions[0], recoveredTx)
		if err != nil {
			return nil, fmt.Errorf("Unable to decode encapsulated transaction: %s", err)
		}
		return recoveredTx, nil
	} else {
		defInxInfo, err := txSetStValue.BlockForIndex(txSetStValue.Index)
		if err != nil {
			return nil, fmt.Errorf("unable to find queried default transaction index info. [%s]", err)
		}
		defBlock := defInxInfo.BlockNr
		inxAtBlock := txSetStValue.Index - defInxInfo.InBlockIndex
		if defBlock < ledger.GetBlockchainSize() {
			// Take the set definition from a previous block
			txIdxMap, err := ledger.blockchain.indexer.fetchTransactionIndexMap(inBlockTx.Txid)
			if err != nil {
				return nil, err
			}
			txInx, ok := txIdxMap[defBlock]
			if !ok {
				return nil, fmt.Errorf("Unable to find given set at its current default block, txdi: %s", inBlockTx.Txid)
			}
			block, err := ledger.GetBlockByNumber(defBlock)
			if err != nil {
				return nil, err
			}
			txSet := block.GetTransactions()[txInx]
			if txSet.GetTransactionSet() == nil {
				return nil, fmt.Errorf("The current default block does not contain a tx set for the given tx id (%s).", inBlockTx.Txid)
			}
			defTxBytes = txSet.GetTransactionSet().Transactions[inxAtBlock]
		}
	}
	if inBlockTx.ConfidentialityLevel == protos.ConfidentialityLevel_CONFIDENTIAL {
		copiedDefTx := make([]byte, len(defTxBytes))
		copy(copiedDefTx, defTxBytes)
		defTxBytes, err = txset.DecryptTxSetSpecification(inBlockTx.Nonce, copiedDefTx, txSetStValue.Index)
		if err != nil {
			return nil, fmt.Errorf("Unable to decrypt transaction specification. Error: [%s]", err)
		}
	}
	transactionSpec := &protos.TxSpec{}
	err = proto.Unmarshal(defTxBytes, transactionSpec)
	if err != nil {
		return nil, fmt.Errorf("Unable to unmarshal default transaction. (%s)", err)
	}
	return container.TransactionFromTxSpec(transactionSpec)
}

// GetTransactionByID return transaction by it's txId
//REVIEW: check whether the txId referred to here is the one of the txSet or the default transaction
func (ledger *Ledger) GetTransactionByID(txID string) (*protos.InBlockTransaction, error) {
	return ledger.blockchain.getTransactionByID(txID)
}

// PutRawBlock puts a raw block on the chain. This function should only be
// used for synchronization between peers.
func (ledger *Ledger) PutRawBlock(block *protos.Block, blockNumber uint64) error {
	err := ledger.blockchain.persistRawBlock(block, blockNumber)
	if err != nil {
		return err
	}
	sendProducerBlockEvent(block)
	return nil
}

// VerifyChain will verify the integrity of the blockchain. This is accomplished
// by ensuring that the previous block hash stored in each block matches
// the actual hash of the previous block in the chain. The return value is the
// block number of lowest block in the range which can be verified as valid.
// The first block is assumed to be valid, and an error is only returned if the
// first block does not exist, or some other sort of irrecoverable ledger error
// such as the first block failing to hash is encountered.
// For example, if VerifyChain(0, 99) is called and previous hash values stored
// in blocks 8, 32, and 42 do not match the actual hashes of respective previous
// block 42 would be the return value from this function.
// highBlock is the high block in the chain to include in verification. If you
// wish to verify the entire chain, use ledger.GetBlockchainSize() - 1.
// lowBlock is the low block in the chain to include in verification. If
// you wish to verify the entire chain, use 0 for the genesis block.
func (ledger *Ledger) VerifyChain(highBlock, lowBlock uint64) (uint64, error) {
	if highBlock >= ledger.GetBlockchainSize() {
		return highBlock, ErrOutOfBounds
	}
	if highBlock < lowBlock {
		return lowBlock, ErrOutOfBounds
	}

	currentBlock, err := ledger.GetBlockByNumber(highBlock)
	if err != nil {
		return highBlock, fmt.Errorf("Error fetching block %d.", highBlock)
	}
	if currentBlock == nil {
		return highBlock, fmt.Errorf("Block %d is nil.", highBlock)
	}

	for i := highBlock; i > lowBlock; i-- {
		previousBlock, err := ledger.GetBlockByNumber(i - 1)
		if err != nil {
			return i, nil
		}
		if previousBlock == nil {
			return i, nil
		}
		previousBlockHash, err := previousBlock.GetHash()
		if err != nil {
			return i, nil
		}
		if bytes.Compare(previousBlockHash, currentBlock.PreviousBlockHash) != 0 {
			return i, nil
		}
		currentBlock = previousBlock
	}

	return lowBlock, nil
}

func (ledger *Ledger) checkValidIDBegin() error {
	if ledger.currentID != nil {
		return fmt.Errorf("Another TxGroup [%s] already in-progress", ledger.currentID)
	}
	return nil
}

func (ledger *Ledger) checkValidIDCommitORRollback(id interface{}) error {
	if !reflect.DeepEqual(ledger.currentID, id) {
		return fmt.Errorf("Another TxGroup [%s] already in-progress", ledger.currentID)
	}
	return nil
}

func (ledger *Ledger) resetForNextTxGroup(txCommited bool) {
	ledgerLogger.Debug("resetting ledger state for next transaction batch")
	ledger.currentID = nil
	ledger.chaincodeState.ClearInMemoryChanges(txCommited)
	ledger.txSetState.ClearInMemoryChanges(txCommited)
}

func sendProducerBlockEvent(block *protos.Block) {

	// Remove payload from deploy transactions. This is done to make block
	// events more lightweight as the payload for these types of transactions
	// can be very large.
	blockTransactions := block.GetTransactions()
	for _, inBlockTx := range blockTransactions {
		switch inBlockTx.Transaction.(type) {
		case *protos.InBlockTransaction_TransactionSet:
			transaction, err := ledger.GetCurrentDefault(inBlockTx, false)
			if err != nil {
				ledgerLogger.Errorf("Error getting the default transaction for set id: %s. Error: %s", inBlockTx.Txid, err)
				continue
			}
			if transaction.Type == protos.ChaincodeAction_CHAINCODE_DEPLOY {
				deploymentSpec := &protos.ChaincodeDeploymentSpec{}
				err := proto.Unmarshal(transaction.Payload, deploymentSpec)
				if err != nil {
					ledgerLogger.Errorf("Error unmarshalling deployment transaction for block event: %s", err)
					continue
				}
				deploymentSpec.CodePackage = nil
				deploymentSpecBytes, err := proto.Marshal(deploymentSpec)
				if err != nil {
					ledgerLogger.Errorf("Error marshalling deployment transaction for block event: %s", err)
					continue
				}
				transaction.Payload = deploymentSpecBytes
			}
		case *protos.InBlockTransaction_MutantTransaction:
			//TODO: generate events for mutable transactions here!
		}
	}

	producer.Send(producer.CreateBlockEvent(block))
}

//send chaincode events created by transactions
func sendChaincodeEvents(trs []*protos.TransactionResult) {
	if trs != nil {
		for _, tr := range trs {
			//we store empty chaincode events in the protobuf repeated array to make protobuf happy.
			//when we replay off a block ignore empty events
			if tr.ChaincodeEvent != nil && tr.ChaincodeEvent.ChaincodeID != "" {
				producer.Send(producer.CreateChaincodeEvent(tr.ChaincodeEvent))
			}
		}
	}
}
