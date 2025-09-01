package tests

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kvstore/engine"
)

func TestWALAppendAndReplay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")

	wal, err := engine.OpenWAL(path)
	require.NoError(t, err)

	entries := []engine.WALEntry{
		{Type: engine.TypeSet, Key: "foo", Value: []byte("bar")},
		{Type: engine.TypeSet, Key: "baz", Value: []byte("qux")},
		{Type: engine.TypeDelete, Key: "foo"},
	}
	for _, e := range entries {
		require.NoError(t, wal.Append(e))
	}
	require.NoError(t, wal.Close())

	wal2, err := engine.OpenWAL(path)
	require.NoError(t, err)
	defer wal2.Close()

	replayed, err := wal2.Replay()
	require.NoError(t, err)
	assert.Equal(t, entries, replayed)
}

func TestWALTornWriteDetection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")

	wal, err := engine.OpenWAL(path)
	require.NoError(t, err)

	for i := 0; i < 10; i++ {
		key := "key" + string(rune('a'+i))
		require.NoError(t, wal.Append(engine.WALEntry{
			Type:  engine.TypeSet,
			Key:   key,
			Value: []byte("value"),
		}))
	}
	require.NoError(t, wal.Close())

	info, err := os.Stat(path)
	require.NoError(t, err)
	f, err := os.OpenFile(path, os.O_WRONLY, 0644)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(info.Size()-5))
	require.NoError(t, f.Close())

	wal2, err := engine.OpenWAL(path)
	require.NoError(t, err)
	defer wal2.Close()

	replayed, err := wal2.Replay()
	require.NoError(t, err)
	assert.Equal(t, 9, len(replayed))
}

func TestWALTruncate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")

	wal, err := engine.OpenWAL(path)
	require.NoError(t, err)

	require.NoError(t, wal.Append(engine.WALEntry{
		Type: engine.TypeSet, Key: "k", Value: []byte("v"),
	}))
	require.NoError(t, wal.Truncate())

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, int64(0), info.Size())

	replayed, err := wal.Replay()
	require.NoError(t, err)
	assert.Empty(t, replayed)
	require.NoError(t, wal.Close())
}

func TestWALChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")

	wal, err := engine.OpenWAL(path)
	require.NoError(t, err)

	for i := 0; i < 5; i++ {
		require.NoError(t, wal.Append(engine.WALEntry{
			Type:  engine.TypeSet,
			Key:   "key",
			Value: []byte("val"),
		}))
	}
	require.NoError(t, wal.Close())

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	// Corrupt CRC of the third entry
	offset := 0
	for i := 0; i < 2; i++ {
		length := binary.BigEndian.Uint32(data[offset : offset+4])
		offset += int(8 + length)
	}
	binary.BigEndian.PutUint32(data[offset+4:offset+8], 0xDEADBEEF)
	require.NoError(t, os.WriteFile(path, data, 0644))

	wal2, err := engine.OpenWAL(path)
	require.NoError(t, err)
	defer wal2.Close()

	replayed, err := wal2.Replay()
	require.NoError(t, err)
	assert.Equal(t, 2, len(replayed))

	assert.Equal(t, "key", replayed[0].Key)
	assert.Equal(t, "key", replayed[1].Key)
	_ = crc32.ChecksumIEEE
}
