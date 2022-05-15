package mvcc

import (
	"bytes"
	"github.com/pingcap-incubator/tinykv/kv/util/engine_util"
)

// Scanner is used for reading multiple sequential key/value pairs from the storage layer. It is aware of the implementation
// of the storage layer and returns results suitable for users.
// Invariant: either the scanner is finished and cannot be used, or it is ready to return a value immediately.
type Scanner struct {
	// Your Data Here (4C).
	iter engine_util.DBIterator
	txn  *MvccTxn
	next []byte
}

// NewScanner creates a new scanner ready to read from the snapshot in txn.
func NewScanner(startKey []byte, txn *MvccTxn) *Scanner {
	// Your Code Here (4C).
	return &Scanner{
		iter: txn.Reader.IterCF(engine_util.CfWrite),
		txn:  txn,
		next: startKey,
	}
}

func (scan *Scanner) Close() {
	// Your Code Here (4C).
	scan.iter.Close()
}

// Next returns the next key/value pair from the scanner. If the scanner is exhausted, then it will return `nil, nil, nil`.
func (scan *Scanner) Next() ([]byte, []byte, error) {
	// Your Code Here (4C).
	if scan.next == nil {
		return nil, nil, nil
	}
	key := scan.next
	scan.iter.Seek(EncodeKey(key, scan.txn.StartTS))
	if !scan.iter.Valid() {
		scan.next = nil
		return nil, nil, nil
	}
	item := scan.iter.Item()
	currUserKey := DecodeUserKey(item.Key())
	// find next key
	for ; scan.iter.Valid(); scan.iter.Next() {
		nextUserKey := DecodeUserKey(scan.iter.Item().Key())
		if !bytes.Equal(nextUserKey, currUserKey) {
			scan.next = nextUserKey
			break
		}
	}
	// no next value
	if !scan.iter.Valid() {
		scan.next = nil
	}
	// get curr value
	value, err := scan.txn.GetValue(currUserKey)
	if err != nil {
		return currUserKey, nil, err
	}
	return currUserKey, value, nil
}
