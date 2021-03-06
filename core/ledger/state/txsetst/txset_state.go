package txsetst

import (
	"fmt"

	"github.com/hyperledger/fabric/core/db"
	"github.com/hyperledger/fabric/core/ledger/state/txsetst/statemgmt"
	//	"github.com/hyperledger/fabric/core/ledger/state/txset_state/buckettree"
	//	"github.com/hyperledger/fabric/core/ledger/state/txset_state/trie"
	stcomm "github.com/hyperledger/fabric/core/ledger/state"
	"github.com/hyperledger/fabric/core/ledger/state/txsetst/raw"
	pb "github.com/hyperledger/fabric/protos"
	"github.com/op/go-logging"
	"github.com/tecbot/gorocksdb"
)

var txSetStateImpl statemgmt.HashableTxSetState

var txSetStateLogger = logging.MustGetLogger("txsetst")

type txSetStateImplType struct {
	name string
}

func (implInt *txSetStateImplType) Name() string {
	return implInt.name
}

var (
	//	buckettreeType txSetStateImplType = "buckettree"
	//	trieType 	   txSetStateImplType = "trie"
	rawType = &txSetStateImplType{"raw"}
)

var defaultTxSetStateImpl = rawType

// TxSetState structure for maintaining world state.
// This encapsulates a particular implementation for managing the state persistence
// This is not thread safe
type TxSetState struct {
	txSetStateImpl         statemgmt.HashableTxSetState
	txSetStateDelta        *statemgmt.TxSetStateDelta
	currentTxSetStateDelta *statemgmt.TxSetStateDelta
	currentTxID            string
	txStateDeltaHash       map[string][]byte
	updateStateImpl        bool
	historyStateDeltaSize  uint64
}

// NewTxSetState constructs a new TxSetState. This Initializes encapsulated state implementation
func NewTxSetState() *TxSetState {
	confData := stcomm.GetConfig("txSetState", defaultTxSetStateImpl, rawType)
	txSetStateLogger.Infof("Initializing tx set state implementation [%s]", confData.StateImplName)
	switch confData.StateImplName {
	/*	case buckettreeType:
			txSetStateImpl = buckettree.NewTxSetStateImpl()
		case trieType:
			txSetStateImpl = trie.NewTxSetStateImpl()*/
	case rawType.Name():
		txSetStateImpl = raw.NewTxSetStateImpl()
	default:
		panic("Should not reach here. Configs should have checked for the txSetStateImplName being a valid names ")
	}
	err := txSetStateImpl.Initialize(confData.StateImplConfigs)
	if err != nil {
		panic(fmt.Errorf("Error during initialization of tx set state implementation: %s", err))
	}
	return &TxSetState{txSetStateImpl, statemgmt.NewTxSetStateDelta(), statemgmt.NewTxSetStateDelta(), "", make(map[string][]byte),
		false, uint64(confData.DeltaHistorySize)}
}

// TxBegin marks begin of a new tx. If a tx is already in progress, this call panics.
// Transactions can cause mutable transactions (triggering a smart contract).
// REVIEW should mutant transactions go directly without calling TxBegin since they clearly modify only one value?
func (state *TxSetState) TxBegin(txID string) {
	txSetStateLogger.Debugf("txBegin() for txId [%s]", txID)
	if state.txInProgress() {
		panic(fmt.Errorf("A tx [%s] is already in progress. Received call for begin of another tx [%s]", state.currentTxID, txID))
	}
	state.currentTxID = txID
}

// TxFinish marks the completion of on-going tx. If txID is not same as of the on-going tx, this call panics
func (state *TxSetState) TxFinish(txID string, txSuccessful bool) {
	txSetStateLogger.Debugf("txFinish() for txId [%s], txSuccessful=[%t]", txID, txSuccessful)
	if state.currentTxID != txID {
		panic(fmt.Errorf("Different txId in tx-begin [%s] and tx-finish [%s]", state.currentTxID, txID))
	}
	if txSuccessful {
		if !state.currentTxSetStateDelta.IsEmpty() {
			txSetStateLogger.Debugf("txFinish() for txId [%s] merging state changes", txID)
			state.txSetStateDelta.ApplyChanges(state.currentTxSetStateDelta)
			state.txStateDeltaHash[txID] = state.currentTxSetStateDelta.ComputeCryptoHash()
			state.updateStateImpl = true
		} else {
			state.txStateDeltaHash[txID] = nil
		}
	}
	state.currentTxSetStateDelta = statemgmt.NewTxSetStateDelta()
	state.currentTxID = ""
}

func (state *TxSetState) txInProgress() bool {
	return state.currentTxID != ""
}

// Get returns state for txID. If committed is false, this first looks in memory and if missing,
// pulls from db. If committed is true, this pulls from the db only.
func (state *TxSetState) Get(txID string, committed bool) (*pb.TxSetStateValue, error) {
	if !committed {
		valueHolder := state.currentTxSetStateDelta.Get(txID)
		if valueHolder != nil {
			return valueHolder.GetValue(), nil
		}
		valueHolder = state.txSetStateDelta.Get(txID)
		if valueHolder != nil {
			return valueHolder.GetValue(), nil
		}
	}
	return state.txSetStateImpl.Get(txID)
}

// Set sets state to given index for the txSetID. Does not immediately writes to DB
func (state *TxSetState) Set(txSetID string, stateValue *pb.TxSetStateValue) error {
	txSetStateLogger.Debugf("set() txSetID=[%s], index=[%d]", txSetID, stateValue.Index)
	// TODO: Do I need to start a transaction if this is primarily called for mutant transactions?
	if !state.txInProgress() {
		panic("State can be changed only in context of a tx.")
	}

	// Check if a previous value is already set in the state delta,
	// if so raise a warning and not change the value. A transactionSet
	// index can be changed only one time per block.
	if state.currentTxSetStateDelta.IsUpdatedValueSet(txSetID) || state.txSetStateDelta.IsUpdatedValueSet(txSetID) {
		txSetStateLogger.Warning("Potential dependency cycle avoided by not changing an already modified tx set value")
		// No need to bother looking up the previous value as we will not
		// set it again. Just pass nil
		return nil
	}

	// Lookup the previous value
	previousValue, err := state.Get(txSetID, true)
	if err != nil {
		return err
	}
	state.currentTxSetStateDelta.Set(txSetID, stateValue, previousValue)

	return nil
}

// Delete tracks the deletion of state for txSetID. Does not immediately write to DB
// REVIEW: Should delete be allowed??
func (state *TxSetState) Delete(txSetID string) error {
	txSetStateLogger.Debugf("delete() txSetID=[%s]", txSetID)
	if !state.txInProgress() {
		panic("State can be changed only in context of a tx.")
	}
	// Need to lookup the previous value
	previousStateValue, err := state.Get(txSetID, true)
	if err != nil {
		return err
	}
	state.currentTxSetStateDelta.Delete(txSetID, previousStateValue)
	return nil
}

// CopyState copies the state from sourceTxSetID to destTxSetID
func (state *TxSetState) CopyState(sourceTxSetID string, destTxSetID string) error {
	sourceValue, err := state.Get(sourceTxSetID, true)
	if err != nil {
		return err
	}
	err = state.Set(destTxSetID, sourceValue)
	if err != nil {
		return err
	}
	return nil
}

func (state *TxSetState) GetOlderBlockMod() (uint64, bool) {
	var older uint64
	var isSet = false
	for _, stID := range state.txSetStateDelta.GetUpdatedTxSetIDs(false) {
		updates := state.txSetStateDelta.GetUpdates(stID)
		if !updates.IsMutant {
			continue
		}
		if !isSet {
			isSet = true
			older = updates.Value.IntroBlock
		}
		if state.txSetStateDelta.GetUpdates(stID).Value.IntroBlock < older {
			older = updates.Value.IntroBlock
		}
	}
	return older, isSet
}

// GetHash computes new state hash if the stateDelta is to be applied.
// Recomputes only if stateDelta has changed after most recent call to this function
func (state *TxSetState) GetHash() ([]byte, error) {
	txSetStateLogger.Debug("Enter - GetHash()")
	if state.updateStateImpl {
		txSetStateLogger.Debug("updating stateImpl with working-set")
		state.txSetStateImpl.PrepareWorkingSet(state.txSetStateDelta)
		state.updateStateImpl = false
	}
	hash, err := state.txSetStateImpl.ComputeCryptoHash()
	if err != nil {
		return nil, err
	}
	txSetStateLogger.Debug("Exit - GetHash()")
	return hash, nil
}

// GetTxStateDeltaHash return the hash of the StateDelta
func (state *TxSetState) GetTxStateDeltaHash() map[string][]byte {
	return state.txStateDeltaHash
}

// ClearInMemoryChanges remove from memory all the changes to state
func (state *TxSetState) ClearInMemoryChanges(changesPersisted bool) {
	state.txSetStateDelta = statemgmt.NewTxSetStateDelta()
	state.txStateDeltaHash = make(map[string][]byte)
	state.txSetStateImpl.ClearWorkingSet(changesPersisted)
}

// getStateDelta get changes in state after most recent call to method clearInMemoryChanges
func (state *TxSetState) getStateDelta() *statemgmt.TxSetStateDelta {
	return state.txSetStateDelta
}

// GetTxSetSnapshot returns a snapshot of the global state for the current block. stateSnapshot.Release()
// must be called once you are done.
func (state *TxSetState) GetTxSetSnapshot(blockNumber uint64, dbSnapshot *gorocksdb.Snapshot) (*stcomm.StateSnapshot, error) {
	itr, err := txSetStateImpl.GetTxSetStateSnapshotIterator(dbSnapshot)
	if err != nil {
		return nil, err
	}
	return stcomm.NewStateSnapshot(blockNumber, itr, dbSnapshot)
}

// FetchStateDeltaFromDB fetches the StateDelta corresponding to given blockNumber
func (state *TxSetState) FetchStateDeltaFromDB(blockNumber uint64) (*statemgmt.TxSetStateDelta, error) {
	stateDeltaBytes, err := db.GetDBHandle().GetFromTxSetStateDeltaCF(stcomm.EncodeStateDeltaKey(blockNumber))
	if err != nil {
		return nil, err
	}
	if stateDeltaBytes == nil {
		return nil, nil
	}
	stateDelta := statemgmt.NewTxSetStateDelta()
	stateDelta.Unmarshal(stateDeltaBytes)
	return stateDelta, nil
}

// AddChangesForPersistence adds key-value pairs to writeBatch
func (state *TxSetState) AddChangesForPersistence(blockNumber uint64, writeBatch *gorocksdb.WriteBatch) {
	txSetStateLogger.Debug("txsetstate.addChangesForPersistence()...start")
	if state.updateStateImpl {
		state.txSetStateImpl.PrepareWorkingSet(state.txSetStateDelta)
		state.updateStateImpl = false
	}
	state.txSetStateImpl.AddChangesForPersistence(writeBatch)

	serializedStateDelta := state.txSetStateDelta.Marshal()
	cf := db.GetDBHandle().TxSetStateDeltaCF
	txSetStateLogger.Debugf("Adding state-delta corresponding to block number[%d]", blockNumber)
	writeBatch.PutCF(cf, stcomm.EncodeStateDeltaKey(blockNumber), serializedStateDelta)
	if blockNumber >= state.historyStateDeltaSize {
		blockNumberToDelete := blockNumber - state.historyStateDeltaSize
		txSetStateLogger.Debugf("Deleting state-delta corresponding to block number[%d]", blockNumberToDelete)
		writeBatch.DeleteCF(cf, stcomm.EncodeStateDeltaKey(blockNumberToDelete))
	} else {
		txSetStateLogger.Debugf("Not deleting previous state-delta. Block number [%d] is smaller than historyStateDeltaSize [%d]",
			blockNumber, state.historyStateDeltaSize)
	}
	txSetStateLogger.Debug("txsetstate.addChangesForPersistence()...finished")
}

// ApplyStateDelta applies already prepared stateDelta to the existing state.
// This is an in memory change only. state.CommitStateDelta must be used to
// commit the state to the DB. This method is to be used in state transfer.
func (state *TxSetState) ApplyStateDelta(delta *statemgmt.TxSetStateDelta) {
	state.txSetStateDelta = delta
	state.updateStateImpl = true
}

// CommitStateDelta commits the changes from state.ApplyStateDelta to the
// DB.
func (state *TxSetState) CommitStateDelta() error {
	if state.updateStateImpl {
		state.txSetStateImpl.PrepareWorkingSet(state.txSetStateDelta)
		state.updateStateImpl = false
	}

	writeBatch := gorocksdb.NewWriteBatch()
	defer writeBatch.Destroy()
	state.txSetStateImpl.AddChangesForPersistence(writeBatch)
	opt := gorocksdb.NewDefaultWriteOptions()
	defer opt.Destroy()
	return db.GetDBHandle().DB.Write(opt, writeBatch)
}

// DeleteState deletes ALL state keys/values from the DB. This is generally
// only used during state synchronization when creating a new state from
// a snapshot.
func (state *TxSetState) DeleteState() error {
	state.ClearInMemoryChanges(false)
	err := db.GetDBHandle().DeleteTxSetState()
	if err != nil {
		txSetStateLogger.Errorf("Error deleting state: %s", err)
	}
	return err
}
