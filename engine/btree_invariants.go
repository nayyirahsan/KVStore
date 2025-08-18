package engine

import "fmt"

// CheckInvariants walks the whole tree and verifies the structural
// invariants the search and scan code rely on. Used by property-based tests;
// never called on the hot path.
//
// Invariants:
//  1. Keys within every node are strictly increasing.
//  2. Internal nodes have exactly len(keys)+1 children.
//  3. Every key in children[i] is < keys[i]; every key in children[i+1]
//     is >= keys[i] (separators route "equal goes right").
//  4. All leaves are at the same depth.
//  5. Every node except the root has at least BTreeOrder/2 - 1 keys
//     (loose lower bound; the root may have as few as 0).
//  6. size equals the number of keys stored in leaves.
func (t *BTree) CheckInvariants() error {
	if t.root == nil {
		return fmt.Errorf("nil root")
	}
	leafDepth := -1
	count := 0
	if err := t.checkNode(t.root, "", "", 0, &leafDepth, &count, true); err != nil {
		return err
	}
	if count != t.size {
		return fmt.Errorf("size mismatch: counted %d leaf keys, size=%d", count, t.size)
	}
	return nil
}

// checkNode verifies node against the half-open key range [lo, hi).
// An empty hi means unbounded; an empty lo is a no-op bound since "" is the
// minimum string. Separators can never legitimately be "" (a separator is
// always some subtree's smallest key promoted from a split median or borrow,
// and "" can only ever sit at the very front of the leftmost leaf), so an
// empty separator is reported as corruption rather than treated as a bound.
func (t *BTree) checkNode(node *BTreeNode, lo, hi string, depth int, leafDepth, count *int, isRoot bool) error {
	minKeys := BTreeOrder/2 - 1
	if !isRoot && len(node.keys) < minKeys {
		return fmt.Errorf("node at depth %d underfull: %d keys < %d", depth, len(node.keys), minKeys)
	}
	for i, k := range node.keys {
		if i > 0 && node.keys[i-1] >= k {
			return fmt.Errorf("keys out of order at depth %d: %q >= %q", depth, node.keys[i-1], k)
		}
		if k < lo {
			return fmt.Errorf("key %q below subtree lower bound %q at depth %d", k, lo, depth)
		}
		if hi != "" && k >= hi {
			return fmt.Errorf("key %q at/above subtree upper bound %q at depth %d", k, hi, depth)
		}
	}

	if node.isLeaf {
		if len(node.values) != len(node.keys) {
			return fmt.Errorf("leaf at depth %d: %d values for %d keys", depth, len(node.values), len(node.keys))
		}
		if *leafDepth == -1 {
			*leafDepth = depth
		} else if depth != *leafDepth {
			return fmt.Errorf("leaf at depth %d, expected %d", depth, *leafDepth)
		}
		*count += len(node.keys)
		return nil
	}

	if len(node.children) != len(node.keys)+1 {
		return fmt.Errorf("internal node at depth %d: %d children for %d keys", depth, len(node.children), len(node.keys))
	}
	for i, child := range node.children {
		childLo, childHi := lo, hi
		if i > 0 {
			childLo = node.keys[i-1]
		}
		if i < len(node.keys) {
			childHi = node.keys[i]
			if childHi == "" {
				return fmt.Errorf("empty separator key at depth %d", depth)
			}
		}
		if err := t.checkNode(child, childLo, childHi, depth+1, leafDepth, count, false); err != nil {
			return err
		}
	}
	return nil
}
