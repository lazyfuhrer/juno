package trie

import (
	"github.com/NethermindEth/juno/db"
	"github.com/NethermindEth/juno/encoder"
	"github.com/bits-and-blooms/bitset"
)

var _ Storage = (*TransactionStorage)(nil)

// TransactionStorage is a database transaction on a trie.
type TransactionStorage struct {
	txn    db.Transaction
	prefix []byte
}

func NewTransactionStorage(txn db.Transaction, prefix []byte) *TransactionStorage {
	return &TransactionStorage{
		txn:    txn,
		prefix: prefix,
	}
}

// dbKey creates a byte array to be used as a key to our KV store
// it simply appends the given key to the configured prefix
func (t *TransactionStorage) dbKey(key *bitset.BitSet) ([]byte, error) {
	keyBytes, err := key.MarshalBinary()
	if err != nil {
		return nil, err
	}

	return append(t.prefix, keyBytes...), nil
}

func (t *TransactionStorage) Put(key *bitset.BitSet, value *Node) error {
	dbKey, err := t.dbKey(key)
	if err != nil {
		return err
	}

	valueBytes, err := encoder.Marshal(value)
	if err != nil {
		return err
	}

	return t.txn.Set(dbKey, valueBytes)
}

func (t *TransactionStorage) Get(key *bitset.BitSet) (*Node, error) {
	dbKey, err := t.dbKey(key)
	if err != nil {
		return nil, err
	}

	var node *Node
	if err = t.txn.Get(dbKey, func(val []byte) error {
		node = new(Node)
		return encoder.Unmarshal(val, node)
	}); err != nil {
		return nil, err
	}
	return node, err
}

func (t *TransactionStorage) Delete(key *bitset.BitSet) error {
	dbKey, err := t.dbKey(key)
	if err != nil {
		return err
	}
	return t.txn.Delete(dbKey)
}