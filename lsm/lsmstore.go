package lsm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"kvstore/engine"
)

var (
	ErrKeyNotFound = errors.New("key not found")
	ErrStoreClosed = errors.New("store is closed")
)

type Store struct {
	mu       sync.RWMutex
	memtable *Memtable
	wal      *engine.WAL
	dir      string
	levels   [][]*SSTable
	seq      uint64
	stats    IOStats
	closed   bool
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	s := &Store{
		memtable: NewMemtable(),
		dir:      dir,
		levels:   make([][]*SSTable, 3),
	}

	manifest, err := loadManifest(dir)
	if err != nil {
		return nil, fmt.Errorf("load manifest: %w", err)
	}
	s.seq = manifest.seq

	referenced := make(map[string]bool, len(manifest.tables))
	for _, entry := range manifest.tables {
		referenced[entry.path] = true
		path := filepath.Join(dir, entry.path)
		sst, err := OpenSSTable(path, entry.level, entry.seq)
		if err != nil {
			return nil, fmt.Errorf("open sstable %s: %w", entry.path, err)
		}
		s.levels[entry.level] = append(s.levels[entry.level], sst)
	}

	// Remove SSTables the manifest doesn't reference and stale temp files:
	// leftovers from a crash between manifest update and input deletion
	// (compaction deletes inputs only after the new manifest is durable).
	if names, err := os.ReadDir(dir); err == nil {
		for _, de := range names {
			name := de.Name()
			isOrphanSST := strings.HasPrefix(name, "sst-") && !strings.HasSuffix(name, ".tmp") && !referenced[name]
			if isOrphanSST || strings.HasSuffix(name, ".tmp") {
				os.Remove(filepath.Join(dir, name))
			}
		}
	}

	walPath := filepath.Join(dir, "wal")
	wal, err := engine.OpenWAL(walPath)
	if err != nil {
		return nil, fmt.Errorf("open wal: %w", err)
	}
	s.wal = wal

	entries, err := wal.Replay()
	if err != nil {
		wal.Close()
		return nil, fmt.Errorf("replay wal: %w", err)
	}
	for _, entry := range entries {
		switch entry.Type {
		case engine.TypeSet:
			s.memtable.Set(entry.Key, entry.Value)
		case engine.TypeDelete:
			s.memtable.Delete(entry.Key)
		}
	}

	return s, nil
}

func (s *Store) checkClosed() error {
	if s.closed {
		return ErrStoreClosed
	}
	return nil
}

func (s *Store) Set(key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkClosed(); err != nil {
		return err
	}
	if err := s.wal.Append(engine.WALEntry{Type: engine.TypeSet, Key: key, Value: value}); err != nil {
		return err
	}
	s.stats.LogicalBytes += uint64(len(key) + len(value))
	s.stats.WALBytes += walRecordSize(key, value)
	s.memtable.Set(key, value)
	if s.memtable.ApproxBytes() >= MaxMemtableSize {
		return s.flushMemtable()
	}
	return nil
}

// walRecordSize is the on-disk footprint of one WAL record:
// 8-byte frame header + 9-byte entry header + key + value.
func walRecordSize(key string, value []byte) uint64 {
	return uint64(17 + len(key) + len(value))
}

// SetBatch applies all pairs with a single WAL fsync (group commit).
func (s *Store) SetBatch(pairs []KVPair) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkClosed(); err != nil {
		return err
	}
	entries := make([]engine.WALEntry, len(pairs))
	for i, p := range pairs {
		entries[i] = engine.WALEntry{Type: engine.TypeSet, Key: p.Key, Value: p.Value}
	}
	if err := s.wal.AppendBatch(entries); err != nil {
		return err
	}
	for _, p := range pairs {
		s.stats.LogicalBytes += uint64(len(p.Key) + len(p.Value))
		s.stats.WALBytes += walRecordSize(p.Key, p.Value)
		s.memtable.Set(p.Key, p.Value)
	}
	if s.memtable.ApproxBytes() >= MaxMemtableSize {
		return s.flushMemtable()
	}
	return nil
}

func (s *Store) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkClosed(); err != nil {
		return err
	}
	if err := s.wal.Append(engine.WALEntry{Type: engine.TypeDelete, Key: key}); err != nil {
		return err
	}
	s.stats.LogicalBytes += uint64(len(key))
	s.stats.WALBytes += walRecordSize(key, nil)
	s.memtable.Delete(key)
	if s.memtable.ApproxBytes() >= MaxMemtableSize {
		return s.flushMemtable()
	}
	return nil
}

func (s *Store) Get(key string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.checkClosed(); err != nil {
		return nil, err
	}

	if val, ok, tombstone := s.memtable.Get(key); ok {
		return val, nil
	} else if tombstone {
		return nil, ErrKeyNotFound
	}

	var best *sstEntry
	for level := 0; level < len(s.levels); level++ {
		for i := len(s.levels[level]) - 1; i >= 0; i-- {
			sst := s.levels[level][i]
			if key < sst.MinKey() || key > sst.MaxKey() {
				continue
			}
			if !sst.MayContain(key) {
				continue
			}
			it := sst.newIterator()
			it.seek(key)
			if !it.done && it.entry.key == key {
				e := it.entry
				if best == nil || e.seq > best.seq {
					best = &e
				}
			}
		}
	}

	if best == nil {
		return nil, ErrKeyNotFound
	}
	if best.tombstone {
		return nil, ErrKeyNotFound
	}
	return best.value, nil
}

func (s *Store) Scan(start, end string) ([]KVPair, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.checkClosed(); err != nil {
		return nil, err
	}

	type scanItem struct {
		key   string
		value []byte
		seq   uint64
		tomb  bool
	}
	best := make(map[string]scanItem)

	for level := 0; level < len(s.levels); level++ {
		for _, sst := range s.levels[level] {
			for _, e := range sst.loadAll() {
				if e.key < start || e.key >= end {
					continue
				}
				if cur, ok := best[e.key]; !ok || e.seq > cur.seq {
					best[e.key] = scanItem{key: e.key, value: e.value, seq: e.seq, tomb: e.tombstone}
				}
			}
		}
	}

	for _, e := range s.memtable.entries {
		if e.key >= start && e.key < end {
			seq := s.seq + 1
			best[e.key] = scanItem{key: e.key, value: e.value, seq: seq, tomb: e.tombstone}
		}
	}

	keys := make([]string, 0, len(best))
	for k, v := range best {
		if !v.tomb {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	result := make([]KVPair, 0, len(keys))
	for _, k := range keys {
		v := best[k]
		result = append(result, KVPair{Key: k, Value: append([]byte(nil), v.value...)})
	}
	return result, nil
}

func (s *Store) Compact() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkClosed(); err != nil {
		return err
	}
	if s.memtable.Len() > 0 {
		if err := s.flushMemtable(); err != nil {
			return err
		}
	}
	if err := s.runCompaction(); err != nil {
		return err
	}
	return s.wal.Truncate()
}

func (s *Store) flushMemtable() error {
	if s.memtable.Len() == 0 {
		return nil
	}

	entries := make([]sstEntry, 0, s.memtable.Len())
	for _, e := range s.memtable.entries {
		entries = append(entries, sstEntry{
			key:       e.key,
			value:     append([]byte(nil), e.value...),
			tombstone: e.tombstone,
			seq:       s.seq,
		})
	}

	s.seq++
	path := sstPath(s.dir, 0, s.seq)
	sst, err := WriteSSTable(path, entries, 0, s.seq)
	if err != nil {
		return fmt.Errorf("flush memtable: %w", err)
	}
	s.stats.FlushBytes += fileSize(path)

	s.levels[0] = append(s.levels[0], sst)
	s.memtable.Reset()

	if err := s.writeManifest(); err != nil {
		return err
	}
	// Everything the WAL guards is now durable in an SSTable referenced by
	// the manifest, so the log can restart from empty. Without this the WAL
	// grows until the next explicit Compact and recovery replays entries
	// that were already flushed.
	if err := s.wal.Truncate(); err != nil {
		return err
	}
	return s.maybeCompactL0()
}

// fileSize returns the on-disk size of path, or 0 if it cannot be measured.
func fileSize(path string) uint64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return uint64(info.Size())
}

// IOStats tracks cumulative bytes for the write-amplification accounting.
// Write amplification = (WALBytes + FlushBytes + CompactBytes) / LogicalBytes.
type IOStats struct {
	LogicalBytes uint64 // key+value bytes the caller asked to store
	WALBytes     uint64 // bytes appended to the write-ahead log
	FlushBytes   uint64 // SSTable bytes written by memtable flushes
	CompactBytes uint64 // SSTable bytes rewritten by compaction
}

// WriteAmplification returns total physical bytes written per logical byte.
func (st IOStats) WriteAmplification() float64 {
	if st.LogicalBytes == 0 {
		return 0
	}
	return float64(st.WALBytes+st.FlushBytes+st.CompactBytes) / float64(st.LogicalBytes)
}

func (s *Store) Stats() IOStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stats
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
