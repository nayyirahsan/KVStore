package engine

import (
	"errors"
	"fmt"
	"os"
	"sync"
)

var (
	ErrKeyNotFound = errors.New("key not found")
	ErrStoreClosed = errors.New("store is closed")
)

type Store struct {
	mu       sync.RWMutex
	tree     *BTree
	wal      *WAL
	snapshot *Snapshot
	dir      string
	closed   bool
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	tree, wal, err := Recover(dir)
	if err != nil {
		return nil, fmt.Errorf("recovery failed: %w", err)
	}
	return &Store{
		tree:     tree,
		wal:      wal,
		snapshot: &Snapshot{dir: dir},
		dir:      dir,
	}, nil
}

func (s *Store) checkClosed() error {
	if s.closed {
		return ErrStoreClosed
	}
	return nil
}

func (s *Store) Get(key string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.checkClosed(); err != nil {
		return nil, err
	}
	val, ok := s.tree.Get(key)
	if !ok {
		return nil, ErrKeyNotFound
	}
	// Copy so the caller can't mutate the tree's internal buffer.
	return append([]byte(nil), val...), nil
}

func (s *Store) Set(key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkClosed(); err != nil {
		return err
	}
	if err := s.wal.Append(WALEntry{Type: TypeSet, Key: key, Value: value}); err != nil {
		return err
	}
	// Copy so a caller reusing its buffer after Set can't corrupt the index.
	s.tree.Set(key, append([]byte(nil), value...))
	return nil
}

// SetBatch applies all pairs with a single WAL fsync (group commit),
// amortizing the dominant per-write cost. Durability semantics: when
// SetBatch returns nil, every pair is durable; on crash mid-batch, recovery
// keeps an intact prefix of the batch.
func (s *Store) SetBatch(pairs []KVPair) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkClosed(); err != nil {
		return err
	}
	entries := make([]WALEntry, len(pairs))
	for i, p := range pairs {
		entries[i] = WALEntry{Type: TypeSet, Key: p.Key, Value: p.Value}
	}
	if err := s.wal.AppendBatch(entries); err != nil {
		return err
	}
	for _, p := range pairs {
		s.tree.Set(p.Key, append([]byte(nil), p.Value...))
	}
	return nil
}

func (s *Store) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkClosed(); err != nil {
		return err
	}
	if err := s.wal.Append(WALEntry{Type: TypeDelete, Key: key}); err != nil {
		return err
	}
	s.tree.Delete(key)
	return nil
}

// Scan returns all pairs with start <= key < end in sorted order.
// An empty end means "no upper bound".
func (s *Store) Scan(start, end string) ([]KVPair, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.checkClosed(); err != nil {
		return nil, err
	}
	pairs := s.tree.Scan(start, end)
	for i := range pairs {
		pairs[i].Value = append([]byte(nil), pairs[i].Value...)
	}
	return pairs, nil
}

func (s *Store) Compact() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkClosed(); err != nil {
		return err
	}
	if err := s.snapshot.Write(s.tree); err != nil {
		return fmt.Errorf("snapshot write: %w", err)
	}
	return s.wal.Truncate()
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.wal.Close()
}

func (s *Store) Dir() string {
	return s.dir
}
