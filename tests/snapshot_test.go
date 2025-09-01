package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kvstore/engine"
)

// A crash in the middle of writing a snapshot leaves a partial snapshot.tmp
// but must never touch the live snapshot: the store recovers from the OLD
// snapshot plus the WAL with no data loss.
func TestCrashMidSnapshotKeepsOldSnapshot(t *testing.T) {
	dir := t.TempDir()

	store, err := engine.Open(dir)
	require.NoError(t, err)
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("key%04d", i)
		require.NoError(t, store.Set(key, []byte(key)))
	}
	require.NoError(t, store.Compact()) // durable snapshot now exists
	for i := 100; i < 150; i++ {
		key := fmt.Sprintf("key%04d", i)
		require.NoError(t, store.Set(key, []byte(key)))
	}
	// Simulate a crash halfway through the NEXT snapshot: a partial temp
	// file exists, the rename never happened. No clean Close.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "snapshot.tmp"),
		[]byte("SNAPSHOT v2\n6b6579\tdeadbe"), 0644))

	store2, err := engine.Open(dir)
	require.NoError(t, err)
	defer store2.Close()
	for i := 0; i < 150; i++ {
		key := fmt.Sprintf("key%04d", i)
		val, err := store2.Get(key)
		require.NoError(t, err, "key %s lost", key)
		assert.Equal(t, []byte(key), val)
	}
}

// A corrupt live snapshot must fail recovery loudly, not silently open an
// empty store on top of it.
func TestCorruptSnapshotFailsLoudly(t *testing.T) {
	dir := t.TempDir()

	store, err := engine.Open(dir)
	require.NoError(t, err)
	for i := 0; i < 50; i++ {
		require.NoError(t, store.Set(fmt.Sprintf("key%04d", i), []byte("v")))
	}
	require.NoError(t, store.Compact())
	require.NoError(t, store.Close())

	snapPath := filepath.Join(dir, "snapshot")
	data, err := os.ReadFile(snapPath)
	require.NoError(t, err)
	data[len(data)/2] ^= 0xFF
	require.NoError(t, os.WriteFile(snapPath, data, 0644))

	_, err = engine.Open(dir)
	require.Error(t, err, "opening on a corrupt snapshot must fail, not lose data silently")
	assert.Contains(t, err.Error(), "checksum")
}

// Binary keys — tabs, newlines, high bytes, and keys above the old
// "\xff\xff\xff\xff" scan sentinel — must survive Compact + recovery.
// Regression test: the v1 snapshot format broke on tab/newline keys, and
// the sentinel-bounded scan silently dropped keys >= "\xff\xff\xff\xff".
func TestBinaryKeysSurviveCompaction(t *testing.T) {
	dir := t.TempDir()

	keys := []string{
		"tab\tkey",
		"newline\nkey",
		"\xff\xff\xff\xff",
		"\xff\xff\xff\xff\xff after sentinel",
		string([]byte{0x00, 0x01, 0xfe}),
		"", // empty key
	}

	store, err := engine.Open(dir)
	require.NoError(t, err)
	for i, k := range keys {
		require.NoError(t, store.Set(k, []byte(fmt.Sprintf("val%d", i))))
	}
	require.NoError(t, store.Compact())
	require.NoError(t, store.Close())

	store2, err := engine.Open(dir)
	require.NoError(t, err)
	defer store2.Close()
	for i, k := range keys {
		val, err := store2.Get(k)
		require.NoError(t, err, "key %q lost through compaction", k)
		assert.Equal(t, []byte(fmt.Sprintf("val%d", i)), val)
	}
}
