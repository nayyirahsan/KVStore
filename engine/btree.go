package engine

import (
	"sort"
)

const BTreeOrder = 32

type BTreeNode struct {
	keys     []string
	values   [][]byte
	children []*BTreeNode
	isLeaf   bool
}

type BTree struct {
	root *BTreeNode
	size int
}

type KVPair struct {
	Key   string
	Value []byte
}

func NewBTree() *BTree {
	return &BTree{
		root: &BTreeNode{isLeaf: true},
	}
}

func (t *BTree) Size() int {
	return t.size
}

func (t *BTree) childIndex(node *BTreeNode, key string) int {
	i := sort.SearchStrings(node.keys, key)
	if i < len(node.keys) && node.keys[i] == key {
		return i + 1
	}
	return i
}

func (t *BTree) Get(key string) ([]byte, bool) {
	node := t.root
	for node != nil && !node.isLeaf {
		node = node.children[t.childIndex(node, key)]
	}
	if node == nil {
		return nil, false
	}
	i := sort.SearchStrings(node.keys, key)
	if i < len(node.keys) && node.keys[i] == key {
		return node.values[i], true
	}
	return nil, false
}

func (t *BTree) Set(key string, value []byte) {
	if len(t.root.keys) == BTreeOrder {
		oldRoot := t.root
		t.root = &BTreeNode{isLeaf: false, children: []*BTreeNode{oldRoot}}
		t.splitChild(t.root, 0)
	}
	t.insert(t.root, key, value)
}

func (t *BTree) insert(node *BTreeNode, key string, value []byte) {
	if node.isLeaf {
		i := sort.SearchStrings(node.keys, key)
		if i < len(node.keys) && node.keys[i] == key {
			node.values[i] = value
			return
		}
		node.keys = append(node.keys, "")
		node.values = append(node.values, nil)
		copy(node.keys[i+1:], node.keys[i:])
		copy(node.values[i+1:], node.values[i:])
		node.keys[i] = key
		node.values[i] = value
		t.size++
		return
	}

	i := t.childIndex(node, key)
	if len(node.children[i].keys) == BTreeOrder {
		t.splitChild(node, i)
		i = t.childIndex(node, key)
	}
	t.insert(node.children[i], key, value)
}

func (t *BTree) splitChild(parent *BTreeNode, index int) {
	full := parent.children[index]
	median := BTreeOrder / 2

	newNode := &BTreeNode{isLeaf: full.isLeaf}

	if full.isLeaf {
		promotedKey := full.keys[median]
		newNode.keys = append([]string(nil), full.keys[median:]...)
		newNode.values = append([][]byte(nil), full.values[median:]...)
		full.keys = full.keys[:median]
		full.values = full.values[:median]

		parent.keys = append(parent.keys, "")
		parent.children = append(parent.children, nil)
		copy(parent.keys[index+1:], parent.keys[index:])
		copy(parent.children[index+2:], parent.children[index+1:])
		parent.keys[index] = promotedKey
		parent.children[index+1] = newNode
		return
	}

	promotedKey := full.keys[median]
	newNode.keys = append([]string(nil), full.keys[median+1:]...)
	newNode.children = append([]*BTreeNode(nil), full.children[median+1:]...)
	full.keys = full.keys[:median]
	full.children = full.children[:median+1]

	parent.keys = append(parent.keys, "")
	parent.children = append(parent.children, nil)
	copy(parent.keys[index+1:], parent.keys[index:])
	copy(parent.children[index+2:], parent.children[index+1:])
	parent.keys[index] = promotedKey
	parent.children[index+1] = newNode
}

func (t *BTree) Delete(key string) bool {
	if t.root == nil {
		return false
	}
	deleted := t.deleteKey(t.root, key)
	if deleted && len(t.root.keys) == 0 && !t.root.isLeaf {
		t.root = t.root.children[0]
	}
	return deleted
}

func (t *BTree) deleteKey(node *BTreeNode, key string) bool {
	if node.isLeaf {
		i := sort.SearchStrings(node.keys, key)
		if i < len(node.keys) && node.keys[i] == key {
			node.keys = append(node.keys[:i], node.keys[i+1:]...)
			node.values = append(node.values[:i], node.values[i+1:]...)
			t.size--
			return true
		}
		return false
	}

	i := t.childIndex(node, key)
	child := node.children[i]

	if len(child.keys) < (BTreeOrder+1)/2 {
		t.fixChildSize(node, i)
		i = t.childIndex(node, key)
		child = node.children[i]
	}
	return t.deleteKey(child, key)
}

func (t *BTree) fixChildSize(node *BTreeNode, i int) {
	minKeys := (BTreeOrder + 1) / 2
	child := node.children[i]

	if i > 0 && len(node.children[i-1].keys) >= minKeys {
		left := node.children[i-1]
		if child.isLeaf {
			child.keys = append([]string{left.keys[len(left.keys)-1]}, child.keys...)
			child.values = append([][]byte{left.values[len(left.values)-1]}, child.values...)
			left.keys = left.keys[:len(left.keys)-1]
			left.values = left.values[:len(left.values)-1]
			// The separator must stay equal to the smallest key of the right
			// child, or lookups for the borrowed key route to the wrong leaf.
			node.keys[i-1] = child.keys[0]
		} else {
			child.keys = append([]string{node.keys[i-1]}, child.keys...)
			child.children = append([]*BTreeNode{left.children[len(left.children)-1]}, child.children...)
			node.keys[i-1] = left.keys[len(left.keys)-1]
			left.keys = left.keys[:len(left.keys)-1]
			left.children = left.children[:len(left.children)-1]
		}
		return
	}

	if i < len(node.children)-1 && len(node.children[i+1].keys) >= minKeys {
		right := node.children[i+1]
		if child.isLeaf {
			child.keys = append(child.keys, right.keys[0])
			child.values = append(child.values, right.values[0])
			right.keys = right.keys[1:]
			right.values = right.values[1:]
			// Keep the separator equal to the right sibling's new smallest key.
			node.keys[i] = right.keys[0]
		} else {
			child.keys = append(child.keys, node.keys[i])
			child.children = append(child.children, right.children[0])
			node.keys[i] = right.keys[0]
			right.keys = right.keys[1:]
			right.children = right.children[1:]
		}
		return
	}

	if i < len(node.children)-1 {
		t.mergeChildren(node, i)
	} else {
		t.mergeChildren(node, i-1)
	}
}

func (t *BTree) mergeChildren(node *BTreeNode, i int) {
	left := node.children[i]
	right := node.children[i+1]

	if !left.isLeaf {
		left.keys = append(left.keys, node.keys[i])
		left.keys = append(left.keys, right.keys...)
		left.children = append(left.children, right.children...)
	} else {
		left.keys = append(left.keys, right.keys...)
		left.values = append(left.values, right.values...)
	}

	node.keys = append(node.keys[:i], node.keys[i+1:]...)
	node.children = append(node.children[:i+1], node.children[i+2:]...)
}

// Scan returns all pairs with start <= key < end in sorted order.
// An empty end means "no upper bound": every key >= start is returned.
func (t *BTree) Scan(start, end string) []KVPair {
	var result []KVPair
	t.scanNode(t.root, start, end, &result)
	return result
}

func (t *BTree) scanNode(node *BTreeNode, start, end string, result *[]KVPair) {
	if node == nil {
		return
	}
	if node.isLeaf {
		for i, key := range node.keys {
			if end != "" && key >= end {
				break
			}
			if key >= start {
				*result = append(*result, KVPair{Key: key, Value: node.values[i]})
			}
		}
		return
	}
	// Separator node.keys[i] is the smallest key in children[i+1], so
	// children[i] holds keys < node.keys[i] and children[i+1] holds
	// keys >= node.keys[i]. Skip subtrees entirely outside [start, end).
	for i := 0; i <= len(node.keys); i++ {
		// Child i holds keys >= node.keys[i-1]; once that reaches end,
		// neither this child nor any later one can contain a match.
		if i > 0 && end != "" && node.keys[i-1] >= end {
			return
		}
		// Child i holds keys < node.keys[i]; if that is <= start, none match.
		if i < len(node.keys) && node.keys[i] <= start {
			continue
		}
		t.scanNode(node.children[i], start, end, result)
	}
}
