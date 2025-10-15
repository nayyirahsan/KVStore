# KVStore

A persistent key-value store written from scratch in Go (core engine is standard-library
only), built to understand how real storage engines earn their durability guarantees.

Two interchangeable engines behind the same API:

- **B-tree engine** (`engine/`): write-ahead log + in-memory B-tree + snapshot-based
  WAL compaction. The default.
- **LSM engine** (`lsm/`): memtable → sorted SSTables in leveled files, with bloom
  filters, sequence-number versioning, tombstones, and k-way merge compaction.

The contract both engines honor: **when `Set` returns nil, the write survives
`kill -9`.** Every claim in this README is enforced by a test, including torn-write
recovery, crash-mid-snapshot, and double-crash scenarios.

```
Get / Set / Delete / Scan / SetBatch / Compact / Close
```

## Quick start

```bash
make test        # full suite
make test-race   # suite under the race detector
make bench       # benchmarks (several minutes; every write fsyncs)

go build -o kvstore ./cli/
./kvstore -dir ./data set name ada
./kvstore -dir ./data get name
./kvstore -dir ./data -engine lsm set name ada   # same CLI, LSM engine
```

As a library:

```go
store, err := engine.Open("./data")   // recovers state: snapshot + WAL replay
defer store.Close()

store.Set("k", []byte("v"))           // durable when this returns
v, err := store.Get("k")
pairs, err := store.Scan("a", "b")    // [start, end), end="" means unbounded
store.Compact()                       // snapshot the tree, truncate the WAL
```

## Architecture

### Write path

```
Set(key, value)
  │
  ├─ 1. acquire write lock (sync.RWMutex)
  ├─ 2. serialize entry → append to WAL file
  ├─ 3. fsync the WAL              ◄── the write is durable HERE
  ├─ 4. update in-memory B-tree    (or memtable, LSM)
  └─ 5. return nil to caller
```

Steps 2–3 happen strictly before step 4. See [design decision #1](#1-wal-before-index)
for why the order is load-bearing.

### Read path

```
B-tree engine                      LSM engine
─────────────                      ──────────
Get(key)                           Get(key)
  │ RLock (readers don't            │ RLock
  │ block each other)               ├─ memtable?  hit/tombstone → done
  └─ walk B-tree (order 32,         └─ for each SSTable, newest level first:
     binary search per node)             ├─ min/max key range check
                                         ├─ bloom filter (10 bits/key, 3 probes)
                                         └─ sparse index → seek → scan;
                                            highest sequence number wins
```

### Recovery path (both engines, every `Open`)

```
Open(dir)
  │
  ├─ 1. load base state
  │      B-tree: read snapshot, verify CRC32 (corrupt snapshot = loud failure)
  │      LSM:    read manifest, open every SSTable it references,
  │             sweep orphaned SSTables + stale .tmp files
  ├─ 2. replay the WAL on top, entry by entry
  │      each record has a length + CRC32 header
  │      first torn/corrupt record → stop, TRUNCATE the log there
  └─ 3. store is open; the tail of history after a torn record is
        exactly the writes that were never acknowledged
```

The truncation in step 2 matters: the WAL is opened in append mode, so if garbage
were left at the tail, post-recovery writes would land *after* it and be invisible
to every subsequent replay. That exact double-crash sequence is a regression test
(`TestTornTailDoesNotPoisonLaterWrites`).

## On-disk formats

### WAL record

```
┌────────────┬────────────┬─────────────────────────────────────────────┐
│ length u32 │  crc32 u32 │                payload                      │
│ big-endian │ of payload │                                             │
└────────────┴────────────┴─────────────────────────────────────────────┘
                           ┌──────┬────────────┬────────────┬─────┬─────┐
                           │ type │ keyLen u32 │ valLen u32 │ key │ val │
                           │  1B  │            │            │     │     │
                           └──────┴────────────┴────────────┴─────┴─────┘
                            type: 0x01 = SET, 0x02 = DELETE (valLen = 0)
```

A record is valid only if its full length is present *and* the CRC matches. A crash
mid-append leaves a record that fails one of those checks; replay keeps everything
before it and truncates the rest.

### Snapshot (B-tree engine)

Line-based, written to `snapshot.tmp`, fsync'd, then atomically renamed over
`snapshot` (then the directory is fsync'd so the rename itself is durable):

```
SNAPSHOT v2
<hex(key)>\t<hex(value)>
...
CHECKSUM <crc32 of header + data lines>
```

Keys and values are both hex-encoded — a key containing `\t` or `\n` must not be able
to corrupt the format (`TestBinaryKeysSurviveCompaction`).

### SSTable (LSM engine)

```
┌───────────────────┬──────────────┬──────────────┬────────────────────┐
│  entries, sorted  │ sparse index │ bloom filter │     footer 16B     │
│  by key           │ (1 per 16    │ (10 bits/key)│ dataLen  indexLen  │
│                   │  entries)    │              │ bloomLen magic     │
└───────────────────┴──────────────┴──────────────┴────────────────────┘
entry: [keyLen u32][valLen u32][flags 1B][seq u64][key][value]
       flags bit 0 = tombstone; seq = flush sequence, newest wins
```

Written to a temp file, fsync'd, renamed, directory fsync'd — same recipe as the
snapshot. The `manifest` file (same atomic-rename recipe) records which SSTables are
live; compaction deletes its input files only *after* the new manifest is durable,
so a crash at any point leaves either the old state or the new state, never a
manifest pointing at deleted files.

## Design decisions

### 1. WAL before index

The WAL append + fsync completes before the in-memory index is touched, and both
happen before `Set` returns. Two failure modes this prevents:

- **Ack-then-lose**: if the index were updated (or the call returned) before the log
  was durable, a crash after the ack would silently lose an acknowledged write —
  the one property a durable store must never violate.
- **Dirty reads of doomed data**: with the index updated first, a concurrent `Get`
  could observe a value whose log write then fails; the value would vanish on
  restart even though a reader saw it. Log-first means anything readable is already
  durable.

### 2. fsync on every write

`write(2)` puts data in the page cache; only `fsync` forces it to stable media. This
store fsyncs inside every `Set` before acknowledging, which makes the fsync *the*
cost of a write: **p50 ≈ 4.0 ms** on this machine, ~260 writes/s (Go's
`File.Sync` issues `F_FULLFSYNC` on macOS, which flushes the drive cache too —
stricter and slower than Linux `fsync`). The B-tree walk contributes nanoseconds;
the durability contributes milliseconds.

The escape hatch is amortization, not weakening the guarantee: `SetBatch` performs
group commit — n writes, one fsync. Batches of 100 sustain **~25k writes/s** on the
same hardware, a ~95× throughput improvement at identical per-batch durability
(a crash mid-batch recovers an intact prefix, since every record is independently
checksummed).

### 3. Atomic snapshot via rename

Compaction writes the full tree to `snapshot.tmp`, fsyncs it, then `rename(2)`s it
over the old snapshot, then fsyncs the directory. POSIX rename is atomic: the
filesystem always holds either the complete old snapshot or the complete new one.
A crash mid-write leaves a stale `.tmp` (ignored and swept) and the old snapshot
intact (`TestCrashMidSnapshotKeepsOldSnapshot`). Crash *between* the snapshot rename
and the WAL truncate is also safe: replaying old WAL entries onto the new snapshot
is idempotent (`Set`/`Delete` are last-writer-wins). The final checksum line catches
silent corruption, and recovery fails loudly rather than opening an incomplete
snapshot as if it were the whole database.

### 4. One RWMutex

Concurrency is a single `sync.RWMutex` per store: `Get`/`Scan` take the read lock
(readers never block each other), `Set`/`Delete`/`Compact` take the write lock.
Chosen deliberately over finer-grained schemes: writes hold the lock across an
fsync, so writes serialize anyway — the write lock isn't the bottleneck, the disk
is. Meanwhile read throughput scales with cores, and the invariant "readers see a
tree that is never mid-mutation" is trivially true instead of subtly true. The
whole suite runs clean under `go test -race`, including a mixed
readers/writers/compactor stress test. The cost — a snapshot write blocks readers
for its duration (~120 ms at 100k keys) — is the first thing I'd fix with MVCC
(below).

### 5. B-tree vs LSM

The same WAL fronts two different indexes, which makes the tradeoff concrete:

|                    | B-tree engine                    | LSM engine                           |
|--------------------|----------------------------------|--------------------------------------|
| Write cost         | fsync-bound (identical)          | fsync-bound (identical)              |
| Read cost          | one tree walk, 83–583 ns         | memtable + bloom-filtered tables     |
| Range scan (1k keys)| **23 µs** — keys sorted in one place | 420 µs — merges every table       |
| Compaction         | rewrite everything (118 ms @100k)| incremental, per-level (13 ms amortized) |
| Write amplification| whole tree per snapshot          | **4.65×** measured                   |
| Deletes            | reclaim immediately              | tombstones until bottom-level merge  |

With a synchronous WAL in front, *ingest speed does not differentiate them* — both
are pinned to fsync latency. The LSM's real advantage shows in compaction behavior:
it rewrites 4.65 bytes per logical byte incrementally, while the B-tree engine
rewrites the entire dataset per snapshot — fine at 100k keys, untenable at 100 GB.
The B-tree wins reads and scans because there's exactly one sorted structure to
consult. That's the actual production tradeoff (PostgreSQL vs RocksDB) reproduced
in miniature.

## Crash-safety matrix

Every window a crash can land in, and why it's safe:

| Crash window                                  | Recovery outcome                                                      |
|-----------------------------------------------|-----------------------------------------------------------------------|
| Mid-WAL-append (torn record)                  | CRC/length check drops exactly the unacknowledged tail; log truncated so later writes can't be shadowed |
| After WAL fsync, before index update          | Entry replays from the log; write survives as acknowledged           |
| Mid-snapshot write                            | `.tmp` discarded; old snapshot + full WAL reconstruct state          |
| After snapshot rename, before WAL truncate    | Old entries replay onto new snapshot; idempotent, same state         |
| Mid-SSTable flush                             | `.tmp` swept; entries still in WAL (truncated only after manifest is durable) |
| After compaction manifest, before input delete| Orphaned SSTables swept at next `Open`                               |
| Bit rot in WAL / snapshot / SSTable           | CRC32 (WAL, snapshot) or magic/bounds checks (SSTable): loud failure or safe prefix, never silent garbage |

## Benchmarks

All numbers from `make bench` on this machine — no synthetic or estimated figures:

> MacBook Pro, Apple M4 Pro (12 cores), 24 GB RAM, Apple NVMe SSD,
> macOS 26.5.1, Go 1.26.4 (`arm64`). Note: Go's `File.Sync` = `F_FULLFSYNC` on
> macOS, so write latencies include a full drive-cache flush; Linux `fsync` on a
> typical NVMe device is a few hundred µs instead.

### Writes (durability included)

| Benchmark               | mean      | p50      | p95      | p99      | throughput   |
|-------------------------|-----------|----------|----------|----------|--------------|
| B-tree Set (sequential) | 3.86 ms   | 3.97 ms  | 4.16 ms  | 5.03 ms  | 259 ops/s    |
| B-tree Set (random)     | 3.79 ms   | 3.97 ms  | 4.09 ms  | 4.52 ms  | 264 ops/s    |
| B-tree SetBatch(100)    | 40.7 µs/op| —        | —        | —        | **24,500 ops/s** |
| LSM Set (sequential)    | 3.78 ms   | 3.98 ms  | 4.10 ms  | 5.00 ms  | 265 ops/s    |
| LSM Set (random)        | 3.78 ms   | 3.98 ms  | 4.09 ms  | 4.16 ms  | 264 ops/s    |
| LSM SetBatch(100)       | 39.6 µs/op| —        | —        | —        | **25,200 ops/s** |

What drives it: the p50 of a single Set (3.97 ms) *is* the F_FULLFSYNC latency —
sequential vs random, B-tree vs LSM, nothing else is visible at this scale. Group
commit (one fsync per 100 writes) recovers ~95× throughput without giving up
when-ack-then-durable.

### Reads (100k-key store)

| Benchmark                | mean    | p50     | p95     | p99     |
|--------------------------|---------|---------|---------|---------|
| B-tree Get (sequential)  | 160 ns  | 83 ns   | 125 ns  | 166 ns  |
| B-tree Get (random)      | 290 ns  | 208 ns  | 417 ns  | 583 ns  |
| LSM Get (sequential)     | 155 ns  | 83 ns   | 125 ns  | 167 ns  |
| LSM Get (random)         | 245 ns  | 167 ns  | 292 ns  | 417 ns  |

Sequential reads ride the CPU cache; random reads pay for cache misses on node
binary searches. LSM SSTables are memory-resident in this implementation (files are
read fully at open), so these numbers measure index traversal, not disk reads.

### Scans, compaction, recovery

| Benchmark                              | result                       |
|----------------------------------------|------------------------------|
| B-tree Scan, 1,000 of 100k keys        | 23.0 µs                      |
| LSM Scan, same                         | 420 µs (merges all tables)   |
| B-tree Compact (snapshot ~100k keys)   | 118.5 ms                     |
| LSM Compact (amortized)                | 13.4 ms ¹                    |
| **WAL replay, 10k entries**            | **7.3 ms  (1.37M entries/s)**|
| **WAL replay, 100k entries**           | **76.1 ms (1.31M entries/s)**|
| **WAL replay, 1M entries**             | **786 ms  (1.27M entries/s)**|
| LSM write amplification (measured)     | **4.65×** ²                  |

¹ First iteration flushes the memtable; later iterations amortize to manifest write
  + WAL truncate (two fsyncs). See `TestLSMWriteAmplificationMeasured` for the
  full-pipeline compaction cost.
² 1.2 MB workload, 32 KB memtable to force multi-level compaction: 1.16× WAL +
  1.18× flush + 2.31× compaction rewrites (`go test ./lsm/ -run WriteAmplification -v`).

Replay throughput is flat across three orders of magnitude (~1.3M entries/s) —
recovery is CPU-bound on parse + tree insert, linear in log size, and the reason
snapshots exist: without compaction, restart time grows with total write history
rather than dataset size.

## Testing

- **Property-based B-tree tests**: hundreds of thousands of random
  set/delete/update ops across seeds, cross-checked against a reference map, plus a
  structural invariant checker (uniform leaf depth, separator bounds, node
  occupancy, strict in-node ordering) run after every workload. This caught a real
  bug: leaf borrow during delete rebalancing failed to update the parent separator,
  making borrowed keys unreachable.
- **Crash simulation**: torn WAL tails, mid-log bit flips, corrupt snapshots,
  partial `.tmp` files, orphaned SSTables, recovery with no clean `Close`, and the
  double-crash (torn tail → recover → write → crash → recover) sequence.
- **Race detection**: full suite under `go test -race`, including concurrent
  readers/writers/deleters/scanners against a running compactor.

## What I'd do differently

- **MVCC snapshots.** The single RWMutex means a 120 ms snapshot blocks all reads.
  Copy-on-write tree nodes (LMDB-style) would let readers pin an immutable root
  while writers build a new version — compaction then reads a frozen tree with no
  lock at all.
- **Sharded locks or a lock-free memtable.** Read scaling is already fine, but
  write-lock hold time includes the fsync. A dedicated WAL-writer goroutine with a
  commit queue would let writers enqueue, group-commit, and wake on fsync completion
  — group commit without requiring callers to batch by hand. The LSM memtable should
  also be a skiplist rather than a sorted array (O(log n) random inserts instead of
  O(n) memmove).
- **Bloom filters on the B-tree snapshot / negative lookups.** The LSM path has
  blooms; point-miss-heavy workloads on the B-tree engine pay a full descent to a
  leaf to learn a key is absent.
- **WAL segments.** One growing file means truncate-on-compact; segmented logs
  (16 MB chunks) allow deleting old segments incrementally and bound replay-buffer
  behavior.
- **Streaming SSTable merges.** Compaction currently materializes merge output in
  memory before writing; the heap-based k-way merge should stream straight to the
  output file so compaction memory is O(1) in table size.
- **Paged, mmap'd SSTables and snapshot.** Everything is memory-resident today,
  which caps dataset size at RAM. Block-oriented tables with an LRU block cache is
  the standard fix.

## Layout

```
engine/     WAL, B-tree (+ invariant checker), snapshot, recovery, store
lsm/        memtable, SSTable, bloom filter, manifest, leveled compaction, store
tests/      cross-cutting: durability, crash sims, property tests, concurrency
bench/      benchmark suite (percentile latencies, replay scaling)
cli/        minimal CLI over either engine
```
