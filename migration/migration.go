package migration

import (
	"bytes"
	"encoding/binary"
	"errors"
	"runtime"
	"sync"

	"github.com/NethermindEth/juno/blockchain"
	"github.com/NethermindEth/juno/core"
	"github.com/NethermindEth/juno/core/felt"
	"github.com/NethermindEth/juno/core/trie"
	"github.com/NethermindEth/juno/db"
	"github.com/NethermindEth/juno/encoder"
	"github.com/NethermindEth/juno/utils"
	"github.com/bits-and-blooms/bitset"
	"github.com/sourcegraph/conc/pool"
)

type Migration interface {
	Before()
	Migrate(db.Transaction, utils.Network) error
}

type MigrationFunc func(db.Transaction, utils.Network) error

// Migrate returns f(txn).
func (f MigrationFunc) Migrate(txn db.Transaction, network utils.Network) error {
	return f(txn, network)
}

// Before is a no-op.
func (f MigrationFunc) Before() {}

// migrations contains a set of migrations that can be applied to a database.
// After making breaking changes to the DB layout, add new migrations to this list.
var migrations = []Migration{
	MigrationFunc(migration0000),
	MigrationFunc(relocateContractStorageRootKeys),
	MigrationFunc(recalculateBloomFilters),
	new(changeTrieNodeEncoding),
	MigrationFunc(calculateBlockCommitments),
}

var ErrCallWithNewTransaction = errors.New("call with new transaction")

func MigrateIfNeeded(targetDB db.DB, network utils.Network) error {
	/*
		Schema version of the targetDB determines which set of migrations need to be applied to the database.
		After a migration is successfully executed, which may update the database, the schema version is incremented
		by 1 by this loop.

		For example;

		Assume an empty DB, which has a schema version of 0. So we start applying the migrations from the
		very first one in the list and increment the schema version as we go. After the loop exists we
		end up with a database which's schema version is len(migrations).

		After running that Juno version for a while, if the user updates its Juno version which adds new
		migrations to the list, MigrateIfNeeded will skip the already applied migrations and only apply the
		new ones. It will be able to do this since the schema version it reads from the database will be
		non-zero and that is what we use to initialise the i loop variable.
	*/
	version, err := SchemaVersion(targetDB)
	if err != nil {
		return err
	}

	for i := version; i < uint64(len(migrations)); i++ {
		migration := migrations[i]
		migration.Before()
		for {
			var migrationErr error
			if dbErr := targetDB.Update(func(txn db.Transaction) error {
				migrationErr = migration.Migrate(txn, network)
				if migrationErr != nil {
					if errors.Is(migrationErr, ErrCallWithNewTransaction) {
						return nil // Run the migration again with a new transaction.
					}
					return migrationErr
				}

				// Migration successful. Bump the version.
				var versionBytes [8]byte
				binary.BigEndian.PutUint64(versionBytes[:], i+1)
				return txn.Set(db.SchemaVersion.Key(), versionBytes[:])
			}); dbErr != nil {
				return dbErr
			} else if migrationErr == nil {
				break
			} else if !errors.Is(migrationErr, ErrCallWithNewTransaction) {
				return migrationErr
			}
		}
	}

	return nil
}

func SchemaVersion(targetDB db.DB) (uint64, error) {
	version := uint64(0)
	txn := targetDB.NewTransaction(false)
	err := txn.Get(db.SchemaVersion.Key(), func(bytes []byte) error {
		version = binary.BigEndian.Uint64(bytes)
		return nil
	})
	if err != nil && !errors.Is(err, db.ErrKeyNotFound) {
		return 0, db.CloseAndWrapOnError(txn.Discard, err)
	}

	return version, db.CloseAndWrapOnError(txn.Discard, nil)
}

// migration0000 makes sure the targetDB is empty
func migration0000(txn db.Transaction, _ utils.Network) error {
	it, err := txn.NewIterator()
	if err != nil {
		return err
	}

	// not empty if valid
	if it.Next() {
		return db.CloseAndWrapOnError(it.Close, errors.New("initial DB should be empty"))
	}
	return it.Close()
}

// relocateContractStorageRootKeys moves contract root keys to new locations and deletes the previous locations.
//
// Before: the key to the root in a contract storage trie was stored at 1+<contractAddress>.
// After: the key to the root of the contract storage trie is stored at 3+<contractAddress>.
//
// This enables us to remove the db.ContractRootKey prefix.
func relocateContractStorageRootKeys(txn db.Transaction, _ utils.Network) error {
	it, err := txn.NewIterator()
	if err != nil {
		return err
	}

	// Modifying the db with txn.Set/Delete while iterating can cause consistency issues,
	// so we do them separately.

	// Iterate over all entries in the old bucket, copying each into memory.
	// Even with millions of contracts, this shouldn't be too expensive.
	oldEntries := make(map[string][]byte)
	oldPrefix := db.Unused.Key()
	var value []byte
	for it.Seek(oldPrefix); it.Valid(); it.Next() {
		// Stop iterating once we're out of the old bucket.
		if !bytes.Equal(it.Key()[:len(oldPrefix)], oldPrefix) {
			break
		}

		oldKey := it.Key()
		value, err = it.Value()
		if err != nil {
			return db.CloseAndWrapOnError(it.Close, err)
		}
		oldEntries[string(oldKey)] = value
	}

	if err = it.Close(); err != nil {
		return err
	}

	// Move the entries to their new locations.
	for oldKey, value := range oldEntries {
		oldKeyBytes := []byte(oldKey)
		contractAddress, found := bytes.CutPrefix(oldKeyBytes, oldPrefix)
		if !found {
			// Should not happen.
			return errors.New("prefix not found")
		}

		if err := txn.Set(db.ContractStorage.Key(contractAddress), value); err != nil {
			return err
		}
		if err := txn.Delete(oldKeyBytes); err != nil {
			return err
		}
	}

	return nil
}

// recalculateBloomFilters updates bloom filters in block headers to match what the most recent implementation expects
func recalculateBloomFilters(txn db.Transaction, _ utils.Network) error {
	blockchain.RegisterCoreTypesToEncoder()
	for blockNumber := uint64(0); ; blockNumber++ {
		block, err := blockchain.BlockByNumber(txn, blockNumber)
		if err != nil {
			if errors.Is(err, db.ErrKeyNotFound) {
				return nil
			}
			return err
		}
		block.EventsBloom = core.EventsBloom(block.Receipts)
		if err = blockchain.StoreBlockHeader(txn, block.Header); err != nil {
			return err
		}
	}
}

// changeTrieNodeEncoding migrates to using a custom encoding for trie nodes
// that minimises memory allocations. Always use new(changeTrieNodeEncoding)
// before calling Before(), otherwise it will panic.
type changeTrieNodeEncoding struct {
	trieNodeBuckets map[db.Bucket]*struct {
		seekTo  []byte
		skipLen int
	}
}

func (m *changeTrieNodeEncoding) Before() {
	m.trieNodeBuckets = map[db.Bucket]*struct {
		seekTo  []byte
		skipLen int
	}{
		db.ClassesTrie: {
			seekTo:  db.ClassesTrie.Key(),
			skipLen: 1,
		},
		db.StateTrie: {
			seekTo:  db.StateTrie.Key(),
			skipLen: 1,
		},
		db.ContractStorage: {
			seekTo:  db.ContractStorage.Key(),
			skipLen: 1 + felt.Bytes,
		},
	}
}

func (m *changeTrieNodeEncoding) Migrate(txn db.Transaction, _ utils.Network) error {
	// If we made n a trie.Node, the encoder would fall back to the custom encoding methods.
	// We instead define a cutom struct to force the encoder to use the default encoding.
	var n struct {
		Value *felt.Felt
		Left  *bitset.BitSet
		Right *bitset.BitSet
	}

	var buf bytes.Buffer
	var updatedNodes uint64

	migrateF := func(it db.Iterator, bucket db.Bucket, seekTo []byte, skipLen int) error {
		bucketPrefix := bucket.Key()
		for it.Seek(seekTo); it.Valid(); it.Next() {
			key := it.Key()
			if !bytes.HasPrefix(key, bucketPrefix) {
				delete(m.trieNodeBuckets, bucket)
				break
			}

			if len(key) == skipLen {
				// this entry is not a TrieNode, it is the root key
				continue
			}

			// We cant fit the entire migration in a single transaction but
			// the actual limit is much higher than 1M. But the more updates
			// you queue on a transaction the more memory it uses. 1M is just
			// an arbitrary number that we can both fit in a transaction and
			// dont use too much memory.
			const updatedNodesBatch = 1_000_000
			if updatedNodes >= updatedNodesBatch {
				m.trieNodeBuckets[bucket].seekTo = key
				return ErrCallWithNewTransaction
			}

			v, err := it.Value()
			if err != nil {
				return err
			}

			if err = encoder.Unmarshal(v, &n); err != nil {
				return err
			}

			coreNode := trie.Node(n)
			if _, err = coreNode.WriteTo(&buf); err != nil {
				return err
			}

			if err = txn.Set(key, buf.Bytes()); err != nil {
				return err
			}
			buf.Reset()

			updatedNodes++
		}
		return nil
	}

	iterator, err := txn.NewIterator()
	if err != nil {
		return err
	}

	for bucket, info := range m.trieNodeBuckets {
		if err := migrateF(iterator, bucket, info.seekTo, info.skipLen); err != nil {
			return db.CloseAndWrapOnError(iterator.Close, err)
		}
	}
	return iterator.Close()
}

// calculateBlockCommitments calculates the txn and event commitments for each block and stores them separately
func calculateBlockCommitments(txn db.Transaction, network utils.Network) error {
	var txnLock sync.RWMutex
	workerPool := pool.New().WithErrors().WithMaxGoroutines(runtime.GOMAXPROCS(0))

	for blockNumber := 0; ; blockNumber++ {
		txnLock.RLock()
		block, err := blockchain.BlockByNumber(txn, uint64(blockNumber))
		txnLock.RUnlock()

		if errors.Is(err, db.ErrKeyNotFound) {
			break
		}

		workerPool.Go(func() error {
			commitments, err := core.VerifyBlockHash(block, network)
			if err != nil {
				return err
			}
			txnLock.Lock()
			defer txnLock.Unlock()
			return blockchain.StoreBlockCommitments(txn, block.Number, commitments)
		})
	}

	return workerPool.Wait()
}
