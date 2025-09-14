package lsm

import (
	"sort"
)

// MaxMemtableSize is the flush threshold. A variable (not const) so tests
// can shrink it to exercise flush/compaction without writing gigabytes.
var MaxMemtableSize = 4 * 1024 * 1024

type memEntry struct {
	key      string
	value    []byte
	tombstone bool
}

type Memtable struct {
	entries []memEntry
	size    int
}

func NewMemtable() *Memtable {
	return &Memtable{}
}

func (m *Memtable) Size() int {
	return m.size
}

func (m *Memtable) Len() int {
	return len(m.entries)
}

func (m *Memtable) Set(key string, value []byte) {
	i := sort.Search(len(m.entries), func(j int) bool {
		return m.entries[j].key >= key
	})
	if i < len(m.entries) && m.entries[i].key == key {
		if m.entries[i].tombstone {
			m.size++
		}
		m.entries[i].value = append([]byte(nil), value...)
		m.entries[i].tombstone = false
		return
	}
	entry := memEntry{key: key, value: append([]byte(nil), value...)}
	m.entries = append(m.entries, entry)
	copy(m.entries[i+1:], m.entries[i:])
	m.entries[i] = entry
	m.size++
}

func (m *Memtable) Delete(key string) {
	i := sort.Search(len(m.entries), func(j int) bool {
		return m.entries[j].key >= key
	})
	if i < len(m.entries) && m.entries[i].key == key {
		if !m.entries[i].tombstone {
			m.size--
		}
		m.entries[i].tombstone = true
		m.entries[i].value = nil
		return
	}
	entry := memEntry{key: key, tombstone: true}
	m.entries = append(m.entries, entry)
	copy(m.entries[i+1:], m.entries[i:])
	m.entries[i] = entry
}

func (m *Memtable) Get(key string) ([]byte, bool, bool) {
	i := sort.Search(len(m.entries), func(j int) bool {
		return m.entries[j].key >= key
	})
	if i < len(m.entries) && m.entries[i].key == key {
		if m.entries[i].tombstone {
			return nil, false, true
		}
		return append([]byte(nil), m.entries[i].value...), true, false
	}
	return nil, false, false
}

func (m *Memtable) Scan(start, end string) []KVPair {
	var result []KVPair
	for _, e := range m.entries {
		if e.key >= end {
			break
		}
		if e.key >= start && !e.tombstone {
			result = append(result, KVPair{Key: e.key, Value: append([]byte(nil), e.value...)})
		}
	}
	return result
}

func (m *Memtable) Reset() {
	m.entries = nil
	m.size = 0
}

func (m *Memtable) ApproxBytes() int {
	n := 0
	for _, e := range m.entries {
		n += len(e.key) + len(e.value) + 8
	}
	return n
}
