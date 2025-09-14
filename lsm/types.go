package lsm

type KVPair struct {
	Key   string
	Value []byte
}

type sstEntry struct {
	key       string
	value     []byte
	tombstone bool
	seq       uint64
}
