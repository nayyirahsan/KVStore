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

// A torn tail must not poison the log for writes made after recovery.
// Sequence: crash leaves a torn final entry -> recover (torn entry dropped)
// -> write new keys -> crash again -> recover. The post-crash writes must
// survive the second recovery. This fails if replay merely skips the torn
// tail without truncating it: the WAL is opened O_APPEND, so new entries
// land after the garbage and the next replay stops before reaching them.
func TestTornTailDoesNotPoisonLaterWrites(t *testing.T) {
	dir := t.TempDir()

	store, err := engine.Open(dir)
	require.NoError(t, err)
	for i := 0; i < 10; i++ {
		require.NoError(t, store.Set(fmt.Sprintf("old%02d", i), []byte("v")))
	}
	require.NoError(t, store.Close())

	walPath := filepath.Join(dir, "wal")
	info, err := os.Stat(walPath)
	require.NoError(t, err)
	f, err := os.OpenFile(walPath, os.O_WRONLY, 0644)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(info.Size()-5))
	require.NoError(t, f.Close())

	// First recovery: exactly the torn final entry is gone.
	store2, err := engine.Open(dir)
	require.NoError(t, err)
	for i := 0; i < 9; i++ {
		_, err := store2.Get(fmt.Sprintf("old%02d", i))
		require.NoError(t, err)
	}
	_, err = store2.Get("old09")
	assert.Equal(t, engine.ErrKeyNotFound, err)

	require.NoError(t, store2.Set("new-after-crash", []byte("important")))
	require.NoError(t, store2.Close())

	// Second recovery: the acknowledged post-crash write must still be there.
	store3, err := engine.Open(dir)
	require.NoError(t, err)
	defer store3.Close()
	val, err := store3.Get("new-after-crash")
	require.NoError(t, err, "write acknowledged after torn-tail recovery was lost")
	assert.Equal(t, []byte("important"), val)
}

// Corruption in the middle of the log (bit rot, not a torn write) must cut
// the log at the corrupt entry; everything after it is unrecoverable and the
// store must still open and accept writes.
func TestMidLogCorruptionRecovery(t *testing.T) {
	dir := t.TempDir()

	store, err := engine.Open(dir)
	require.NoError(t, err)
	for i := 0; i < 20; i++ {
		require.NoError(t, store.Set(fmt.Sprintf("key%02d", i), []byte("v")))
	}
	require.NoError(t, store.Close())

	// Flip a byte roughly in the middle of the file.
	walPath := filepath.Join(dir, "wal")
	data, err := os.ReadFile(walPath)
	require.NoError(t, err)
	data[len(data)/2] ^= 0xFF
	require.NoError(t, os.WriteFile(walPath, data, 0644))

	store2, err := engine.Open(dir)
	require.NoError(t, err)
	defer store2.Close()

	// A prefix of keys survives; the store is writable and new writes stick.
	_, err = store2.Get("key00")
	require.NoError(t, err)
	require.NoError(t, store2.Set("post-corruption", []byte("ok")))
	require.NoError(t, store2.Close())

	store3, err := engine.Open(dir)
	require.NoError(t, err)
	defer store3.Close()
	_, err = store3.Get("post-corruption")
	require.NoError(t, err)
}

// Simulated crash: no Close(), just abandon the handles and reopen the
// directory. Every acknowledged Set must be present. (Close() only closes
// the file descriptor — durability must come from the per-write fsync, not
// from a clean shutdown.)
func TestRecoveryWithoutCleanClose(t *testing.T) {
	dir := t.TempDir()

	store, err := engine.Open(dir)
	require.NoError(t, err)
	const n = 500
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("key%06d", i)
		require.NoError(t, store.Set(key, []byte(key)))
	}
	// No Close: the *os.File stays open, as after a SIGKILL.

	store2, err := engine.Open(dir)
	require.NoError(t, err)
	defer store2.Close()
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("key%06d", i)
		val, err := store2.Get(key)
		require.NoError(t, err, "key %s lost", key)
		assert.Equal(t, []byte(key), val)
	}
}

// Same, with a Compact() in the middle: state = snapshot + post-snapshot WAL.
func TestRecoveryWithoutCleanCloseAfterCompact(t *testing.T) {
	dir := t.TempDir()

	store, err := engine.Open(dir)
	require.NoError(t, err)
	for i := 0; i < 300; i++ {
		key := fmt.Sprintf("key%06d", i)
		require.NoError(t, store.Set(key, []byte(key)))
	}
	require.NoError(t, store.Compact())
	for i := 300; i < 400; i++ {
		key := fmt.Sprintf("key%06d", i)
		require.NoError(t, store.Set(key, []byte(key)))
	}
	require.NoError(t, store.Delete("key000000"))
	// No Close.

	store2, err := engine.Open(dir)
	require.NoError(t, err)
	defer store2.Close()
	for i := 1; i < 400; i++ {
		key := fmt.Sprintf("key%06d", i)
		val, err := store2.Get(key)
		require.NoError(t, err, "key %s lost", key)
		assert.Equal(t, []byte(key), val)
	}
	_, err = store2.Get("key000000")
	assert.Equal(t, engine.ErrKeyNotFound, err, "delete after compact must survive recovery")
}
