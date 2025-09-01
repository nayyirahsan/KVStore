package tests

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kvstore/engine"
)

// checkAgainstReference asserts the tree and a reference map hold exactly the
// same data: every ref key Gets the right value, a full scan is sorted and
// complete, size matches, and structural invariants hold.
func checkAgainstReference(t *testing.T, tree *engine.BTree, ref map[string]string) {
	t.Helper()
	require.NoError(t, tree.CheckInvariants())
	require.Equal(t, len(ref), tree.Size(), "tree size diverged from reference")

	for k, v := range ref {
		got, ok := tree.Get(k)
		require.True(t, ok, "key %q in reference but not in tree", k)
		require.Equal(t, []byte(v), got, "wrong value for key %q", k)
	}

	pairs := tree.Scan("", "")
	require.Equal(t, len(ref), len(pairs), "full scan returned wrong number of keys")
	for i := 1; i < len(pairs); i++ {
		require.Less(t, pairs[i-1].Key, pairs[i].Key, "scan output not strictly sorted at index %d", i)
	}
	for _, p := range pairs {
		require.Equal(t, ref[p.Key], string(p.Value))
	}
}

// Random Set/Delete/update workload cross-checked against a map. The small
// key space forces heavy overwrite and delete-rebalance traffic (borrows and
// merges), which is what broke the original implementation.
func TestBTreePropertyRandomOps(t *testing.T) {
	for _, seed := range []int64{1, 2, 3, 42, 1000} {
		seed := seed
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			tree := engine.NewBTree()
			ref := map[string]string{}

			for op := 0; op < 100000; op++ {
				k := fmt.Sprintf("k%04d", rng.Intn(2000))
				if rng.Intn(3) == 0 {
					tree.Delete(k)
					delete(ref, k)
				} else {
					v := fmt.Sprintf("v%d", op)
					tree.Set(k, []byte(v))
					ref[k] = v
				}
			}
			checkAgainstReference(t, tree, ref)

			// Drain to empty through the delete/rebalance path.
			for k := range ref {
				require.True(t, tree.Delete(k))
			}
			require.Equal(t, 0, tree.Size())
			require.NoError(t, tree.CheckInvariants())
			assert.Empty(t, tree.Scan("", ""))
		})
	}
}

// Sorted, reverse-sorted, and random insertion orders must all produce a
// valid tree (cascading splits up to the root included: 50k keys at order 32
// is a tree of depth 4) whose in-order scan returns every key sorted.
func TestBTreeInsertionOrders(t *testing.T) {
	const n = 50000
	permRng := rand.New(rand.NewSource(7))
	cases := map[string][]int{
		"ascending":  seq(n, false),
		"descending": seq(n, true),
		"random":     permRng.Perm(n),
	}
	for name, order := range cases {
		name, order := name, order
		t.Run(name, func(t *testing.T) {
			tree := engine.NewBTree()
			for _, i := range order {
				k := fmt.Sprintf("key%08d", i)
				tree.Set(k, []byte(k))
			}
			require.NoError(t, tree.CheckInvariants())
			require.Equal(t, n, tree.Size())

			pairs := tree.Scan("", "")
			require.Len(t, pairs, n)
			for i, p := range pairs {
				require.Equal(t, fmt.Sprintf("key%08d", i), p.Key, "scan out of order at %d", i)
			}
		})
	}
}

func seq(n int, desc bool) []int {
	out := make([]int, n)
	for i := range out {
		if desc {
			out[i] = n - 1 - i
		} else {
			out[i] = i
		}
	}
	return out
}

// Random range scans compared against a sorted reference slice.
func TestBTreeRangeScanProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	tree := engine.NewBTree()
	ref := map[string]bool{}
	for i := 0; i < 20000; i++ {
		k := fmt.Sprintf("k%05d", rng.Intn(30000))
		tree.Set(k, []byte(k))
		ref[k] = true
	}
	sorted := make([]string, 0, len(ref))
	for k := range ref {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	for trial := 0; trial < 500; trial++ {
		a := fmt.Sprintf("k%05d", rng.Intn(30000))
		b := fmt.Sprintf("k%05d", rng.Intn(30000))
		if a > b {
			a, b = b, a
		}
		want := []string{}
		for _, k := range sorted {
			if k >= a && k < b {
				want = append(want, k)
			}
		}
		got := tree.Scan(a, b)
		require.Len(t, got, len(want), "range [%q, %q)", a, b)
		for i, p := range got {
			require.Equal(t, want[i], p.Key, "range [%q, %q) mismatch at %d", a, b, i)
		}
	}
}

func TestBTreeScanBoundaries(t *testing.T) {
	tree := engine.NewBTree()

	// Empty tree.
	assert.Empty(t, tree.Scan("", ""))
	assert.Empty(t, tree.Scan("a", "z"))

	for _, k := range []string{"b", "d", "f"} {
		tree.Set(k, []byte(k))
	}

	// start == end and start > end are empty.
	assert.Empty(t, tree.Scan("d", "d"))
	assert.Empty(t, tree.Scan("f", "b"))
	// Bounds land exactly on keys: start inclusive, end exclusive.
	got := tree.Scan("b", "f")
	require.Len(t, got, 2)
	assert.Equal(t, "b", got[0].Key)
	assert.Equal(t, "d", got[1].Key)
	// Empty end means unbounded.
	assert.Len(t, tree.Scan("d", ""), 2)
	assert.Len(t, tree.Scan("", ""), 3)
	// Bounds between keys.
	got = tree.Scan("c", "e")
	require.Len(t, got, 1)
	assert.Equal(t, "d", got[0].Key)
}

// The exact shape that broke the original code: force leaf borrows via
// deletes and verify the borrowed keys stay reachable.
func TestBTreeDeleteRebalanceKeepsKeysReachable(t *testing.T) {
	for seed := int64(0); seed < 20; seed++ {
		rng := rand.New(rand.NewSource(seed))
		tree := engine.NewBTree()
		present := map[string]bool{}
		// Build a multi-level tree, then delete two thirds in random order.
		keys := make([]string, 3000)
		for i := range keys {
			keys[i] = fmt.Sprintf("key%06d", i)
			tree.Set(keys[i], []byte("v"))
			present[keys[i]] = true
		}
		for _, i := range rng.Perm(len(keys))[:2000] {
			require.True(t, tree.Delete(keys[i]), "seed %d: delete %q claimed key absent", seed, keys[i])
			delete(present, keys[i])
		}
		require.NoError(t, tree.CheckInvariants(), "seed %d", seed)
		for k := range present {
			_, ok := tree.Get(k)
			require.True(t, ok, "seed %d: key %q lost after delete rebalancing", seed, k)
		}
	}
}
