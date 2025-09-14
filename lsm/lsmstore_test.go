package lsm

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := Open(dir)
	require.NoError(t, err)
	return store, dir
}

func TestLSMSetAndGet(t *testing.T) {
	store, _ := openTestStore(t)
	defer store.Close()

	require.NoError(t, store.Set("hello", []byte("world")))
	val, err := store.Get("hello")
	require.NoError(t, err)
	assert.Equal(t, []byte("world"), val)
}

func TestLSMDelete(t *testing.T) {
	store, _ := openTestStore(t)
	defer store.Close()

	require.NoError(t, store.Set("key", []byte("val")))
	require.NoError(t, store.Delete("key"))
	_, err := store.Get("key")
	assert.Equal(t, ErrKeyNotFound, err)
}

func TestLSMScan(t *testing.T) {
	store, _ := openTestStore(t)
	defer store.Close()

	for _, k := range []string{"a", "b", "c", "d"} {
		require.NoError(t, store.Set(k, []byte(k)))
	}
	pairs, err := store.Scan("b", "d")
	require.NoError(t, err)
	require.Len(t, pairs, 2)
	assert.Equal(t, "b", pairs[0].Key)
	assert.Equal(t, "c", pairs[1].Key)
}

func TestLSMCrashRecovery(t *testing.T) {
	dir := t.TempDir()

	store, err := Open(dir)
	require.NoError(t, err)
	for i := 0; i < 500; i++ {
		key := fmt.Sprintf("key%06d", i)
		require.NoError(t, store.Set(key, []byte(key)))
	}
	require.NoError(t, store.Close())

	store2, err := Open(dir)
	require.NoError(t, err)
	defer store2.Close()

	for i := 0; i < 500; i++ {
		key := fmt.Sprintf("key%06d", i)
		val, err := store2.Get(key)
		require.NoError(t, err, "key %s", key)
		assert.Equal(t, []byte(key), val)
	}
}

func TestLSMCompact(t *testing.T) {
	store, dir := openTestStore(t)
	defer store.Close()

	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("key%06d", i)
		require.NoError(t, store.Set(key, []byte(key)))
	}
	require.NoError(t, store.Compact())

	walPath := filepath.Join(dir, "wal")
	info, err := os.Stat(walPath)
	require.NoError(t, err)
	assert.Equal(t, int64(0), info.Size())

	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("key%06d", i)
		val, err := store.Get(key)
		require.NoError(t, err)
		assert.Equal(t, []byte(key), val)
	}
}

func TestLSMMemtableFlush(t *testing.T) {
	store, _ := openTestStore(t)
	defer store.Close()

	// Write enough data to trigger memtable growth (may not hit 4MB in test, but flush via Compact works)
	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("flush%06d", i)
		require.NoError(t, store.Set(key, []byte(key)))
	}
	require.NoError(t, store.Compact())

	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("flush%06d", i)
		_, err := store.Get(key)
		require.NoError(t, err)
	}
}
