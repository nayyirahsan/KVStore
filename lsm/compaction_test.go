package lsm

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMergeSSTables(t *testing.T) {
	dir := t.TempDir()

	sst1, err := WriteSSTable(filepath.Join(dir, "a.sst"), []sstEntry{
		{key: "a", value: []byte("1"), seq: 1},
		{key: "c", value: []byte("old"), seq: 1},
	}, 0, 1)
	require.NoError(t, err)

	sst2, err := WriteSSTable(filepath.Join(dir, "b.sst"), []sstEntry{
		{key: "b", value: []byte("2"), seq: 2},
		{key: "c", value: []byte("new"), seq: 2},
	}, 0, 2)
	require.NoError(t, err)

	merged, err := mergeSSTables(dir, []*SSTable{sst1, sst2}, 1, 3)
	require.NoError(t, err)

	entries := merged.loadAll()
	require.Len(t, entries, 3)
	assert.Equal(t, "a", entries[0].key)
	assert.Equal(t, "b", entries[1].key)
	assert.Equal(t, "c", entries[2].key)
	assert.Equal(t, []byte("new"), entries[2].value)
}

func TestCompactionDropsTombstones(t *testing.T) {
	dir := t.TempDir()

	sst, err := WriteSSTable(filepath.Join(dir, "del.sst"), []sstEntry{
		{key: "gone", tombstone: true, seq: 1},
		{key: "stay", value: []byte("yes"), seq: 1},
	}, 1, 1)
	require.NoError(t, err)

	merged, err := mergeSSTables(dir, []*SSTable{sst}, 2, 2)
	require.NoError(t, err)

	entries := merged.loadAll()
	require.Len(t, entries, 1)
	assert.Equal(t, "stay", entries[0].key)
}

// Measures real write amplification: shrink the memtable so a ~1.2MB
// workload forces many flushes and multi-level compactions, then compare
// physical bytes written (WAL + flushes + compaction rewrites) to logical
// bytes. Run with -v to see the measured factor.
func TestLSMWriteAmplificationMeasured(t *testing.T) {
	old := MaxMemtableSize
	MaxMemtableSize = 32 * 1024
	defer func() { MaxMemtableSize = old }()

	store, _ := openTestStore(t)
	defer store.Close()

	// Batched writes: identical WAL/flush/compaction byte counts to
	// one-at-a-time Sets, without 10k individual fsyncs slowing the test.
	value := make([]byte, 100)
	for i := 0; i < 10000; i += 100 {
		batch := make([]KVPair, 100)
		for j := range batch {
			batch[j] = KVPair{Key: fmt.Sprintf("wa%06d", i+j), Value: value}
		}
		require.NoError(t, store.SetBatch(batch))
	}
	require.NoError(t, store.Compact())

	st := store.Stats()
	waf := st.WriteAmplification()
	t.Logf("logical=%d WAL=%d flush=%d compact=%d WAF=%.2f",
		st.LogicalBytes, st.WALBytes, st.FlushBytes, st.CompactBytes, waf)
	assert.Greater(t, waf, 1.0, "physical writes must exceed logical writes (WAL + flush)")
	assert.Less(t, waf, 30.0, "write amplification unreasonably high")
	assert.NotEmpty(t, store.levels[1][0:], "workload should have reached L1+")

	// Everything must still be readable after all that churn.
	for i := 0; i < 10000; i += 97 {
		_, err := store.Get(fmt.Sprintf("wa%06d", i))
		require.NoError(t, err)
	}
}

// Orphan SSTables and temp files (a crash between the durable manifest
// update and input-file deletion) must be swept at Open without touching
// live data.
func TestLSMOpenSweepsOrphans(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	require.NoError(t, err)
	for i := 0; i < 200; i++ {
		require.NoError(t, store.Set(fmt.Sprintf("key%04d", i), []byte("v")))
	}
	require.NoError(t, store.Compact())
	require.NoError(t, store.Close())

	orphan := filepath.Join(dir, "sst-L0-999999")
	tmp := filepath.Join(dir, "sst-L1-000003.tmp")
	require.NoError(t, os.WriteFile(orphan, []byte("stale compaction input"), 0644))
	require.NoError(t, os.WriteFile(tmp, []byte("partial write"), 0644))

	store2, err := Open(dir)
	require.NoError(t, err)
	defer store2.Close()

	_, err = os.Stat(orphan)
	assert.True(t, os.IsNotExist(err), "orphan sstable should be swept")
	_, err = os.Stat(tmp)
	assert.True(t, os.IsNotExist(err), "temp file should be swept")
	for i := 0; i < 200; i++ {
		_, err := store2.Get(fmt.Sprintf("key%04d", i))
		require.NoError(t, err)
	}
}

// After an automatic memtable flush the WAL must be empty (its contents are
// durable in an SSTable), and a crash right after must lose nothing.
func TestLSMWALTruncatedAfterAutoFlush(t *testing.T) {
	old := MaxMemtableSize
	MaxMemtableSize = 16 * 1024
	defer func() { MaxMemtableSize = old }()

	dir := t.TempDir()
	store, err := Open(dir)
	require.NoError(t, err)

	value := make([]byte, 128)
	n := 0
	for ; store.Stats().FlushBytes == 0; n++ {
		require.NoError(t, store.Set(fmt.Sprintf("key%06d", n), value))
	}
	require.NotEmpty(t, store.levels[0], "flush should have produced an L0 table")

	info, err := os.Stat(filepath.Join(dir, "wal"))
	require.NoError(t, err)
	assert.Equal(t, int64(0), info.Size(), "WAL should be truncated after flush")

	// A couple more writes land in the fresh WAL; crash without Close.
	require.NoError(t, store.Set("after-flush", []byte("x")))

	store2, err := Open(dir)
	require.NoError(t, err)
	defer store2.Close()
	for i := 0; i < n; i++ {
		_, err := store2.Get(fmt.Sprintf("key%06d", i))
		require.NoError(t, err, "key %d lost across flush + crash", i)
	}
	_, err = store2.Get("after-flush")
	require.NoError(t, err)
}
