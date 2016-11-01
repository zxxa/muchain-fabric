package protos

import (
	"fmt"

	"github.com/golang/protobuf/proto"
	"bytes"
	"errors"
	"sort"
	"reflect"
)

// IsValidBlockExtension checks whether the other txSetStateValue is a valid extension of this txSetStateValue blockwise
// meaning it only adds new blocks or nothing, and the txNumber is consistent with the total number of transactions
// declared
func (txSetStateValue *TxSetStateValue) IsValidBlockExtension(other *TxSetStateValue) error {
	if txSetStateValue.TxNumber > other.TxNumber {
		return fmt.Errorf("The next state for this transactions set contains less transactions. "+
			"Number of transactions info at current state: %d; other state: %d", txSetStateValue.TxNumber, other.TxNumber)
	}
	if nextTxInx := other.IndexAtBlock[len(other.IndexAtBlock) - 1].InBlockIndex; nextTxInx != other.TxNumber-1  {
		return fmt.Errorf("The index of the new set is not correct. Expected: [%d], Actual: [%d]", other.TxNumber-1, nextTxInx)
	}
	if nextBlock := other.IndexAtBlock[len(other.IndexAtBlock) - 1].BlockNr; nextBlock != other.LastModifiedAtBlock {
		return fmt.Errorf("The block of the new set is not correct. Expected: [%d], Actual: [%d]", other.LastModifiedAtBlock, nextBlock)
	}
	for i, indexInfo := range txSetStateValue.IndexAtBlock {
		if indexInfo.BlockNr != other.IndexAtBlock[i].BlockNr || indexInfo.InBlockIndex != other.IndexAtBlock[i].InBlockIndex {
			return fmt.Errorf("The next state for this transactions set contains conflicting index information at " +
				"IndexAtBlock[%d]. Previous: Block[%d], StartInx[%d], next: Block[%d], StartInx[%d].", i, indexInfo.BlockNr, indexInfo.InBlockIndex, other.IndexAtBlock[i].BlockNr, other.IndexAtBlock[i].InBlockIndex)
		}
	}
	if txSetStateValue.IntroBlock != 0 && other.Index != txSetStateValue.Index {
		return errors.New("It is not possible to modify the index in a set extension.")
	}
	return nil
}

func (txSetStateValue *TxSetStateValue) IsValidMutation(other *TxSetStateValue) error {
	if txSetStateValue.LastModifiedAtBlock >= other.LastModifiedAtBlock {
		return fmt.Errorf("It is not allow to modify a transaction before the last time it was modified. Block last time modified: [%d], Current modifying block: [%d]", txSetStateValue.LastModifiedAtBlock, other.LastModifiedAtBlock)
	}
	if txSetStateValue.TxNumber != other.TxNumber {
		return errors.New("A mutant transaction cannot extend a set.")
	}
	if txSetStateValue.Index == other.Index {
		return errors.New("Mutating, but the active index did not change.")
	}
	if other.Index >= other.TxNumber {
		return fmt.Errorf("Provided an out of bound new index for the transaction. Num transactions: [%d], provided new index: [%d]", other.TxNumber, other.Index)
	}
	if !reflect.DeepEqual(txSetStateValue.IndexAtBlock, other.IndexAtBlock) {
		return errors.New("A mutant transaction cannot extend a set.")
	}
	return nil
}

func (txSetStateValue *TxSetStateValue) PositionForIndex(inx uint64) (int, error) {
	i := sort.Search(len(txSetStateValue.IndexAtBlock), func(i int) bool { return inx <= txSetStateValue.IndexAtBlock[i].InBlockIndex})
	if i < len(txSetStateValue.IndexAtBlock) {
		return i, nil
	} else {
		return i, fmt.Errorf("Block for index [%d] not found.", inx)
	}
}

// Bytes returns this block as an array of bytes.
func (txStateValue *TxSetStateValue) Bytes() ([]byte, error) {
	data, err := proto.Marshal(txStateValue)
	if err != nil {
		return nil, fmt.Errorf("Could not marshal txSetStateValue: %s", err)
	}
	return data, nil
}

func (txSetStVal *TxSetStateValue) ToString() string {
	var buffer bytes.Buffer
	buffer.WriteString(fmt.Sprintln("Nonce:", txSetStVal.Nonce))
	buffer.WriteString(fmt.Sprintln("Introduced at block number:", txSetStVal.IntroBlock))
	buffer.WriteString(fmt.Sprintln("Last modified at block number:", txSetStVal.LastModifiedAtBlock))
	buffer.WriteString(fmt.Sprintln("Active transaction index:", txSetStVal.Index))
	buffer.WriteString(fmt.Sprintln("Number of transactions in the set:", txSetStVal.TxNumber))
	buffer.WriteString(fmt.Sprintln("Number of transactions belonging to this set at a given block:"))
	buffer.WriteString(fmt.Sprintln("Block\t\t\tLast Index"))
	for _, inx := range txSetStVal.IndexAtBlock {
		buffer.WriteString(fmt.Sprint(inx.BlockNr, "\t\t\t", inx.InBlockIndex, "\n"))
	}
	return buffer.String()
}

// UnmarshalTxSetStateValue converts a byte array generated by Bytes() back to a block.
func UnmarshalTxSetStateValue(marshalledState []byte) (*TxSetStateValue, error) {
	stateValue := &TxSetStateValue{}
	err := proto.Unmarshal(marshalledState, stateValue)
	if err != nil {
		return nil, fmt.Errorf("Could not unmarshal txSetStateValue: %s", err)
	}
	return stateValue, nil
}
