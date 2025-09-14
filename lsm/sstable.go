package lsm

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

const (
	sstMagic       = 0x4B565354 // "KVST"
	indexInterval  = 16
	bloomBitsPerKey = 10
)

type indexEntry struct {
	key    string
	offset uint32
}

type SSTable struct {
	path       string
	level      int
	minKey     string
	maxKey     string
	seq        uint64
	data       []byte
	index      []indexEntry
	bloom      *bloomFilter
}

type sstIterator struct {
	sst   *SSTable
	pos   int
	entry sstEntry
	done  bool
}

func WriteSSTable(path string, entries []sstEntry, level int, seq uint64) (*SSTable, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("cannot write empty sstable")
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].key < entries[j].key
	})

	var data []byte
	var index []indexEntry
	bloom := newBloomFilter(len(entries), bloomBitsPerKey)

	for i, e := range entries {
		offset := uint32(len(data))
		if i%indexInterval == 0 {
			index = append(index, indexEntry{key: e.key, offset: offset})
		}
		bloom.Add(e.key)

		keyBytes := []byte(e.key)
		var valueLen uint32
		flags := byte(0)
		if e.tombstone {
			flags = 1
		} else {
			valueLen = uint32(len(e.value))
		}

		buf := make([]byte, 4+4+1+8+len(keyBytes)+int(valueLen))
		binary.BigEndian.PutUint32(buf[0:4], uint32(len(keyBytes)))
		binary.BigEndian.PutUint32(buf[4:8], valueLen)
		buf[8] = flags
		binary.BigEndian.PutUint64(buf[9:17], e.seq)
		copy(buf[17:], keyBytes)
		if !e.tombstone {
			copy(buf[17+len(keyBytes):], e.value)
		}
		data = append(data, buf...)
	}

	indexData := encodeIndex(index)
	bloomData := encodeBloom(bloom)

	footer := make([]byte, 16)
	binary.BigEndian.PutUint32(footer[0:4], uint32(len(data)))
	binary.BigEndian.PutUint32(footer[4:8], uint32(len(indexData)))
	binary.BigEndian.PutUint32(footer[8:12], uint32(len(bloomData)))
	binary.BigEndian.PutUint32(footer[12:16], sstMagic)

	tmpPath := path + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return nil, err
	}
	for _, chunk := range [][]byte{data, indexData, bloomData, footer} {
		if _, err := f.Write(chunk); err != nil {
			f.Close()
			os.Remove(tmpPath)
			return nil, err
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return nil, err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return nil, err
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return nil, err
	}

	return OpenSSTable(path, level, seq)
}

func OpenSSTable(path string, level int, seq uint64) (*SSTable, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < 16 {
		return nil, fmt.Errorf("sstable too small")
	}

	footer := raw[len(raw)-16:]
	if binary.BigEndian.Uint32(footer[12:16]) != sstMagic {
		return nil, fmt.Errorf("invalid sstable magic")
	}

	dataLen := binary.BigEndian.Uint32(footer[0:4])
	indexLen := binary.BigEndian.Uint32(footer[4:8])
	bloomLen := binary.BigEndian.Uint32(footer[8:12])

	dataEnd := int(dataLen)
	indexEnd := dataEnd + int(indexLen)
	bloomEnd := indexEnd + int(bloomLen)

	data := raw[:dataEnd]
	index := decodeIndex(raw[dataEnd:indexEnd])
	bloom := decodeBloom(raw[indexEnd:bloomEnd])

	sst := &SSTable{
		path:  path,
		level: level,
		seq:   seq,
		data:  data,
		index: index,
		bloom: bloom,
	}

	if entries := sst.loadAll(); len(entries) > 0 {
		sst.minKey = entries[0].key
		sst.maxKey = entries[len(entries)-1].key
	}
	return sst, nil
}

func encodeIndex(index []indexEntry) []byte {
	var buf []byte
	for _, e := range index {
		keyBytes := []byte(e.key)
		entry := make([]byte, 4+4+len(keyBytes))
		binary.BigEndian.PutUint32(entry[0:4], e.offset)
		binary.BigEndian.PutUint32(entry[4:8], uint32(len(keyBytes)))
		copy(entry[8:], keyBytes)
		buf = append(buf, entry...)
	}
	return buf
}

func decodeIndex(data []byte) []indexEntry {
	var index []indexEntry
	offset := 0
	for offset < len(data) {
		if offset+8 > len(data) {
			break
		}
		dataOffset := binary.BigEndian.Uint32(data[offset : offset+4])
		keyLen := int(binary.BigEndian.Uint32(data[offset+4 : offset+8]))
		offset += 8
		key := string(data[offset : offset+keyLen])
		offset += keyLen
		index = append(index, indexEntry{key: key, offset: dataOffset})
	}
	return index
}

func (s *SSTable) MayContain(key string) bool {
	return s.bloom.MayContain(key)
}

func (s *SSTable) loadAll() []sstEntry {
	var entries []sstEntry
	it := s.newIterator()
	for it.Next() {
		entries = append(entries, it.entry)
	}
	return entries
}

func (s *SSTable) newIterator() *sstIterator {
	return &sstIterator{sst: s, pos: -1}
}

func (it *sstIterator) seek(key string) {
	it.pos = -1
	it.done = false
	startPos := 0
	idx := sort.Search(len(it.sst.index), func(i int) bool {
		return it.sst.index[i].key >= key
	})
	if idx > 0 {
		startPos = int(it.sst.index[idx-1].offset)
	} else if idx < len(it.sst.index) {
		startPos = int(it.sst.index[idx].offset)
	}

	offset := startPos
	for offset < len(it.sst.data) {
		entry, next, err := decodeEntry(it.sst.data, offset)
		if err != nil {
			break
		}
		if entry.key >= key {
			it.pos = offset
			it.entry = entry
			return
		}
		offset = next
	}
	it.done = true
}

func (it *sstIterator) Next() bool {
	if it.done {
		return false
	}
	if it.pos < 0 {
		it.pos = 0
	} else {
		_, next, err := decodeEntry(it.sst.data, it.pos)
		if err != nil {
			it.done = true
			return false
		}
		it.pos = next
		if it.pos >= len(it.sst.data) {
			it.done = true
			return false
		}
	}

	entry, _, err := decodeEntry(it.sst.data, it.pos)
	if err != nil {
		it.done = true
		return false
	}
	it.entry = entry
	return true
}

func decodeEntry(data []byte, offset int) (sstEntry, int, error) {
	if offset+17 > len(data) {
		return sstEntry{}, offset, fmt.Errorf("truncated entry")
	}
	keyLen := int(binary.BigEndian.Uint32(data[offset : offset+4]))
	valueLen := int(binary.BigEndian.Uint32(data[offset+4 : offset+8]))
	flags := data[offset+8]
	seq := binary.BigEndian.Uint64(data[offset+9 : offset+17])

	entryEnd := offset + 17 + keyLen + valueLen
	if entryEnd > len(data) {
		return sstEntry{}, offset, fmt.Errorf("truncated entry payload")
	}

	key := string(data[offset+17 : offset+17+keyLen])
	entry := sstEntry{key: key, seq: seq, tombstone: flags == 1}
	if !entry.tombstone {
		entry.value = append([]byte(nil), data[offset+17+keyLen:entryEnd]...)
	}
	return entry, entryEnd, nil
}

func (s *SSTable) Path() string   { return s.path }
func (s *SSTable) Level() int     { return s.level }
func (s *SSTable) Seq() uint64    { return s.seq }
func (s *SSTable) MinKey() string { return s.minKey }
func (s *SSTable) MaxKey() string { return s.maxKey }

func sstFilename(level int, seq uint64) string {
	return fmt.Sprintf("sst-L%d-%06d", level, seq)
}

func sstPath(dir string, level int, seq uint64) string {
	return filepath.Join(dir, sstFilename(level, seq))
}
