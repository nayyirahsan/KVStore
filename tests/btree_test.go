package tests

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kvstore/engine"
)

func TestBTreeInsertAndGet(t *testing.T) {
	tree := engine.NewBTree()
	tree.Set("apple", []byte("red"))
	tree.Set("banana", []byte("yellow"))

	val, ok := tree.Get("apple")
	require.True(t, ok)
	assert.Equal(t, []byte("red"), val)

	_, ok = tree.Get("cherry")
	assert.False(t, ok)
}

func TestBTreeUpdate(t *testing.T) {
	tree := engine.NewBTree()
	tree.Set("key", []byte("v1"))
	tree.Set("key", []byte("v2"))

	val, ok := tree.Get("key")
	require.True(t, ok)
	assert.Equal(t, []byte("v2"), val)
	assert.Equal(t, 1, tree.Size())
}

func TestBTreeDelete(t *testing.T) {
	tree := engine.NewBTree()
	tree.Set("a", []byte("1"))
	tree.Set("b", []byte("2"))
	tree.Set("c", []byte("3"))

	assert.True(t, tree.Delete("b"))
	_, ok := tree.Get("b")
	assert.False(t, ok)
	assert.Equal(t, 2, tree.Size())

	assert.False(t, tree.Delete("missing"))
}

func TestBTreeScan(t *testing.T) {
	tree := engine.NewBTree()
	keys := []string{"a", "b", "c", "d", "e"}
	for _, k := range keys {
		tree.Set(k, []byte(k+"-val"))
	}

	result := tree.Scan("b", "e")
	require.Len(t, result, 3)
	assert.Equal(t, "b", result[0].Key)
	assert.Equal(t, "c", result[1].Key)
	assert.Equal(t, "d", result[2].Key)
}

func TestBTreeSplit(t *testing.T) {
	tree := engine.NewBTree()
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("key%05d", i)
		tree.Set(key, []byte(key))
	}

	assert.Equal(t, 100, tree.Size())
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("key%05d", i)
		val, ok := tree.Get(key)
		require.True(t, ok, "missing key %s", key)
		assert.Equal(t, []byte(key), val)
	}
}

func TestBTreeDeleteMany(t *testing.T) {
	tree := engine.NewBTree()
	for i := 0; i < 50; i++ {
		tree.Set(fmt.Sprintf("k%03d", i), []byte("v"))
	}
	for i := 0; i < 50; i += 2 {
		tree.Delete(fmt.Sprintf("k%03d", i))
	}
	assert.Equal(t, 25, tree.Size())
	for i := 1; i < 50; i += 2 {
		_, ok := tree.Get(fmt.Sprintf("k%03d", i))
		assert.True(t, ok)
	}
}
