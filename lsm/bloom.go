package lsm

import (
	"encoding/binary"
	"hash/crc32"
	"hash/fnv"
)

type bloomFilter struct {
	bits   []byte
	nBits  uint32
	nKeys  int
}

func newBloomFilter(nKeys int, bitsPerKey int) *bloomFilter {
	nBits := uint32(nKeys * bitsPerKey)
	if nBits < 64 {
		nBits = 64
	}
	return &bloomFilter{
		bits:  make([]byte, (nBits+7)/8),
		nBits: nBits,
		nKeys: nKeys,
	}
}

func (b *bloomFilter) hashes(key string) [3]uint32 {
	h1 := fnv.New32a()
	h1.Write([]byte(key))
	hash1 := h1.Sum32()

	hash2 := crc32.ChecksumIEEE([]byte(key))

	hash3 := hash1 ^ (hash2 * 0x9e3779b1)
	return [3]uint32{hash1, hash2, hash3}
}

func (b *bloomFilter) Add(key string) {
	for _, h := range b.hashes(key) {
		pos := h % b.nBits
		b.bits[pos/8] |= 1 << (pos % 8)
	}
}

func (b *bloomFilter) MayContain(key string) bool {
	for _, h := range b.hashes(key) {
		pos := h % b.nBits
		if b.bits[pos/8]&(1<<(pos%8)) == 0 {
			return false
		}
	}
	return true
}

func encodeBloom(b *bloomFilter) []byte {
	buf := make([]byte, 8+len(b.bits))
	binary.BigEndian.PutUint32(buf[0:4], b.nBits)
	binary.BigEndian.PutUint32(buf[4:8], uint32(b.nKeys))
	copy(buf[8:], b.bits)
	return buf
}

func decodeBloom(data []byte) *bloomFilter {
	nBits := binary.BigEndian.Uint32(data[0:4])
	nKeys := int(binary.BigEndian.Uint32(data[4:8]))
	return &bloomFilter{
		bits:  append([]byte(nil), data[8:]...),
		nBits: nBits,
		nKeys: nKeys,
	}
}
