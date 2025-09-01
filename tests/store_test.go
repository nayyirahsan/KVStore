package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kvstore/engine"
)

func openTestStore(t *testing.T) (*engine.Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := engine.Open(dir)
	require.NoError(t, err)
	return store, dir
}

func TestSetAndGet(t *testing.T) {
	store, _ := openTestStore(t)
	defer store.Close()

	require.NoError(t, store.Set("hello", []byte("world")))
	val, err := store.Get("hello")
	require.NoError(t, err)
	assert.Equal(t, []byte("world"), val)
}

func TestGetMissingKey(t *testing.T) {
	store, _ := openTestStore(t)
	defer store.Close()

	_, err := store.Get("missing")
	assert.Equal(t, engine.ErrKeyNotFound, err)
}

func TestDelete(t *testing.T) {
	store, _ := openTestStore(t)
	defer store.Close()

	require.NoError(t, store.Set("key", []byte("val")))
	require.NoError(t, store.Delete("key"))
	_, err := store.Get("key")
	assert.Equal(t, engine.ErrKeyNotFound, err)
}

func TestDeleteMissingKey(t *testing.T) {
	store, _ := openTestStore(t)
	defer store.Close()

	require.NoError(t, store.Delete("missing"))
}

func TestScanRange(t *testing.T) {
	store, _ := openTestStore(t)
	defer store.Close()

	for _, k := range []string{"a", "b", "c", "d", "e"} {
		require.NoError(t, store.Set(k, []byte(k)))
	}

	pairs, err := store.Scan("b", "e")
	require.NoError(t, err)
	require.Len(t, pairs, 3)
	assert.Equal(t, "b", pairs[0].Key)
	assert.Equal(t, "c", pairs[1].Key)
	assert.Equal(t, "d", pairs[2].Key)
}

func TestScanEmptyRange(t *testing.T) {
	store, _ := openTestStore(t)
	defer store.Close()

	require.NoError(t, store.Set("a", []byte("1")))
	pairs, err := store.Scan("z", "zz")
	require.NoError(t, err)
	assert.Empty(t, pairs)
}

func TestCrashRecovery(t *testing.T) {
	dir := t.TempDir()

	store, err := engine.Open(dir)
	require.NoError(t, err)
	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("key%06d", i)
		require.NoError(t, store.Set(key, []byte(key)))
	}
	require.NoError(t, store.Close())

	store2, err := engine.Open(dir)
	require.NoError(t, err)
	defer store2.Close()

	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("key%06d", i)
		val, err := store2.Get(key)
		require.NoError(t, err, "key %s", key)
		assert.Equal(t, []byte(key), val)
	}
}

func TestCrashRecoveryAfterCompact(t *testing.T) {
	dir := t.TempDir()

	store, err := engine.Open(dir)
	require.NoError(t, err)
	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("key%06d", i)
		require.NoError(t, store.Set(key, []byte(key)))
	}
	require.NoError(t, store.Compact())
	for i := 1000; i < 1500; i++ {
		key := fmt.Sprintf("key%06d", i)
		require.NoError(t, store.Set(key, []byte(key)))
	}
	require.NoError(t, store.Close())

	store2, err := engine.Open(dir)
	require.NoError(t, err)
	defer store2.Close()

	for i := 0; i < 1500; i++ {
		key := fmt.Sprintf("key%06d", i)
		val, err := store2.Get(key)
		require.NoError(t, err, "key %s", key)
		assert.Equal(t, []byte(key), val)
	}
}

func TestTornWriteRecovery(t *testing.T) {
	dir := t.TempDir()

	store, err := engine.Open(dir)
	require.NoError(t, err)
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("key%06d", i)
		require.NoError(t, store.Set(key, []byte(key)))
	}
	require.NoError(t, store.Close())

	walPath := filepath.Join(dir, "wal")
	info, err := os.Stat(walPath)
	require.NoError(t, err)
	f, err := os.OpenFile(walPath, os.O_WRONLY, 0644)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(info.Size()-5))
	require.NoError(t, f.Close())

	store2, err := engine.Open(dir)
	require.NoError(t, err)
	defer store2.Close()

	for i := 0; i < 99; i++ {
		key := fmt.Sprintf("key%06d", i)
		val, err := store2.Get(key)
		require.NoError(t, err, "key %s", key)
		assert.Equal(t, []byte(key), val)
	}
	_, err = store2.Get("key000099")
	assert.Equal(t, engine.ErrKeyNotFound, err)

	require.NoError(t, store2.Set("newkey", []byte("newval")))
	val, err := store2.Get("newkey")
	require.NoError(t, err)
	assert.Equal(t, []byte("newval"), val)
}

func TestCompactTruncatesWAL(t *testing.T) {
	store, dir := openTestStore(t)
	defer store.Close()

	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("key%06d", i)
		require.NoError(t, store.Set(key, []byte(key)))
	}

	require.NoError(t, store.Compact())

	walPath := filepath.Join(dir, "wal")
	info, err := os.Stat(walPath)
	require.NoError(t, err)
	assert.Equal(t, int64(0), info.Size())

	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("key%06d", i)
		val, err := store.Get(key)
		require.NoError(t, err)
		assert.Equal(t, []byte(key), val)
	}
}

func TestConcurrentReads(t *testing.T) {
	store, _ := openTestStore(t)
	defer store.Close()

	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("key%06d", i)
		require.NoError(t, store.Set(key, []byte(key)))
	}

	var wg sync.WaitGroup
	for g := 0; g < 50; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				key := fmt.Sprintf("key%06d", (id*100+j)%1000)
				_, err := store.Get(key)
				assert.NoError(t, err)
			}
		}(g)
	}
	wg.Wait()
}
