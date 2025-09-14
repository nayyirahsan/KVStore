package lsm

import (
	"container/heap"
	"fmt"
	"os"
)

const (
	maxL0Files     = 4
	maxL1Files     = 4
	l1Fanout       = 10
)

type mergeItem struct {
	entry sstEntry
	it    *sstIterator
	idx   int
}

type mergeHeap []*mergeItem

func (h mergeHeap) Len() int           { return len(h) }
func (h mergeHeap) Less(i, j int) bool { return h[i].entry.key < h[j].entry.key }
func (h mergeHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *mergeHeap) Push(x any) {
	*h = append(*h, x.(*mergeItem))
}

func (h *mergeHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

func mergeSSTables(dir string, tables []*SSTable, outLevel int, outSeq uint64) (*SSTable, error) {
	h := &mergeHeap{}
	heap.Init(h)

	for i, sst := range tables {
		it := sst.newIterator()
		if it.Next() {
			heap.Push(h, &mergeItem{entry: it.entry, it: it, idx: i})
		}
	}

	var merged []sstEntry
	var lastKey string
	var lastEntry *sstEntry

	for h.Len() > 0 {
		item := heap.Pop(h).(*mergeItem)
		entry := item.entry

		if entry.key != lastKey {
			if lastEntry != nil {
				merged = append(merged, *lastEntry)
			}
			e := entry
			lastEntry = &e
			lastKey = entry.key
		} else if entry.seq > lastEntry.seq {
			e := entry
			lastEntry = &e
		}

		if item.it.Next() {
			item.entry = item.it.entry
			heap.Push(h, item)
		}
	}
	if lastEntry != nil {
		merged = append(merged, *lastEntry)
	}

	// Drop tombstones at max level or when safe
	if outLevel >= 2 {
		filtered := merged[:0]
		for _, e := range merged {
			if !e.tombstone {
				filtered = append(filtered, e)
			}
		}
		merged = filtered
	}

	outPath := sstPath(dir, outLevel, outSeq)
	return WriteSSTable(outPath, merged, outLevel, outSeq)
}

func (s *Store) maybeCompactL0() error {
	if len(s.levels[0]) < maxL0Files {
		return nil
	}
	return s.compactLevel(0)
}

func (s *Store) compactLevel(level int) error {
	if level >= 2 {
		return nil
	}

	input := append([]*SSTable(nil), s.levels[level]...)
	if len(input) == 0 {
		return nil
	}

	outLevel := level + 1
	s.seq++
	out, err := mergeSSTables(s.dir, input, outLevel, s.seq)
	if err != nil {
		return fmt.Errorf("merge sstables: %w", err)
	}
	s.stats.CompactBytes += fileSize(out.Path())

	s.levels[level] = nil
	s.levels[outLevel] = append(s.levels[outLevel], out)

	// Make the new state durable BEFORE deleting the inputs. If we crash
	// here the old files are merely orphaned (and unreferenced); deleting
	// first would leave the manifest pointing at files that no longer exist
	// and recovery would fail permanently.
	if err := s.writeManifest(); err != nil {
		return fmt.Errorf("write manifest after compaction: %w", err)
	}
	for _, sst := range input {
		os.Remove(sst.Path())
	}

	if outLevel == 1 && len(s.levels[1]) >= maxL1Files {
		return s.compactLevel(1)
	}
	return nil
}

func (s *Store) runCompaction() error {
	if len(s.levels[0]) > 0 {
		if err := s.compactLevel(0); err != nil {
			return err
		}
	}
	if len(s.levels[1]) >= maxL1Files {
		if err := s.compactLevel(1); err != nil {
			return err
		}
	}
	return s.writeManifest()
}
