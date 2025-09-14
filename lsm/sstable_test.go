package lsm

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSSTableWriteAndRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.sst")

	entries := []sstEntry{
		{key: "apple", value: []byte("red"), seq: 1},
		{key: "banana", value: []byte("yellow"), seq: 1},
		{key: "cherry", value: []byte("dark"), seq: 2},
	}

	sst, err := WriteSSTable(path, entries, 0, 1)
	require.NoError(t, err)

	assert.True(t, sst.MayContain("apple"))
	assert.True(t, sst.MayContain("banana"))
	assert.False(t, sst.MayContain("missing"))

	it := sst.newIterator()
	it.seek("banana")
	require.False(t, it.done)
	assert.Equal(t, "banana", it.entry.key)
	assert.Equal(t, []byte("yellow"), it.entry.value)
}

func TestBloomFilter(t *testing.T) {
	bf := newBloomFilter(100, 10)
	bf.Add("foo")
	bf.Add("bar")

	assert.True(t, bf.MayContain("foo"))
	assert.True(t, bf.MayContain("bar"))
	assert.False(t, bf.MayContain("baz"))
}
