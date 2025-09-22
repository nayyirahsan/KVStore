package bench

import (
	"fmt"
	"math/rand"
	"os"
	"sort"
	"testing"
	"time"

	"kvstore/engine"
	"kvstore/lsm"
)

var btreeStore *engine.Store
var lsmStore *lsm.Store
var benchDir string

// Prepopulation sizes. Prepopulation goes through SetBatch (one fsync per
// 10k keys), so these can be realistic without making the harness crawl.
const (
	benchPrepopGet     = 100000
	benchPrepopScan    = 100000
	benchPrepopCompact = 10000
	valueSize          = 20 // "benchmark-value-data"
)

func TestMain(m *testing.M) {
	var err error
	benchDir, err = os.MkdirTemp("", "kvstore-bench-*")
	if err != nil {
		panic(err)
	}

	btreeStore, err = engine.Open(benchDir + "/btree")
	if err != nil {
		panic(err)
	}
	lsmStore, err = lsm.Open(benchDir + "/lsm")
	if err != nil {
		panic(err)
	}

	code := m.Run()

	btreeStore.Close()
	lsmStore.Close()
	os.RemoveAll(benchDir)
	os.Exit(code)
}

func keySeq(i int) string {
	return fmt.Sprintf("key%06d", i)
}

func randomKey(rng *rand.Rand) string {
	b := make([]byte, 16)
	rng.Read(b)
	return string(b)
}

// latencies records per-op durations and reports p50/p95/p99 alongside the
// standard ns/op mean. Per-op time.Now() calls add ~40ns of overhead, which
// is negligible for fsync-bound writes and small relative to reads.
type latencies struct {
	samples []time.Duration
}

func (l *latencies) time(f func()) {
	start := time.Now()
	f()
	l.samples = append(l.samples, time.Since(start))
}

func (l *latencies) report(b *testing.B) {
	if len(l.samples) == 0 {
		return
	}
	sort.Slice(l.samples, func(i, j int) bool { return l.samples[i] < l.samples[j] })
	pct := func(p float64) float64 {
		idx := int(p * float64(len(l.samples)-1))
		return float64(l.samples[idx].Nanoseconds())
	}
	b.ReportMetric(pct(0.50), "p50-ns")
	b.ReportMetric(pct(0.95), "p95-ns")
	b.ReportMetric(pct(0.99), "p99-ns")
}

func benchSetSequential(b *testing.B, setFn func(string, []byte) error) {
	value := []byte("benchmark-value-data")
	lat := &latencies{samples: make([]time.Duration, 0, b.N)}
	b.SetBytes(int64(len(value) + 10))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lat.time(func() {
			if err := setFn(keySeq(i), value); err != nil {
				b.Fatal(err)
			}
		})
	}
	lat.report(b)
}

func benchSetRandom(b *testing.B, setFn func(string, []byte) error) {
	value := []byte("benchmark-value-data")
	rng := rand.New(rand.NewSource(42))
	lat := &latencies{samples: make([]time.Duration, 0, b.N)}
	b.SetBytes(int64(len(value) + 16))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := randomKey(rng)
		lat.time(func() {
			if err := setFn(key, value); err != nil {
				b.Fatal(err)
			}
		})
	}
	lat.report(b)
}

// benchSetBatch measures group-commit throughput: batches of batchSize
// writes, one fsync per batch.
func benchSetBatch(b *testing.B, setBatchFn func([]lsm.KVPair) error, batchSize int) {
	value := []byte("benchmark-value-data")
	b.SetBytes(int64((len(value) + 10) * batchSize))
	batch := make([]lsm.KVPair, batchSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := range batch {
			batch[j] = lsm.KVPair{Key: keySeq(i*batchSize + j), Value: value}
		}
		if err := setBatchFn(batch); err != nil {
			b.Fatal(err)
		}
	}
}

func prepopulate(setBatchFn func([]lsm.KVPair) error, n int) {
	const chunk = 10000
	for lo := 0; lo < n; lo += chunk {
		hi := lo + chunk
		if hi > n {
			hi = n
		}
		batch := make([]lsm.KVPair, 0, hi-lo)
		for i := lo; i < hi; i++ {
			key := keySeq(i)
			batch = append(batch, lsm.KVPair{Key: key, Value: []byte(key)})
		}
		if err := setBatchFn(batch); err != nil {
			panic(err)
		}
	}
}

func btreeSetBatch(pairs []lsm.KVPair) error {
	converted := make([]engine.KVPair, len(pairs))
	for i, p := range pairs {
		converted[i] = engine.KVPair{Key: p.Key, Value: p.Value}
	}
	return btreeStore.SetBatch(converted)
}

func benchGetSequential(b *testing.B, getFn func(string) ([]byte, error), setBatchFn func([]lsm.KVPair) error, n int) {
	prepopulate(setBatchFn, n)
	lat := &latencies{samples: make([]time.Duration, 0, b.N)}
	b.SetBytes(10)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := keySeq(i % n)
		lat.time(func() {
			if _, err := getFn(key); err != nil {
				b.Fatal(err)
			}
		})
	}
	lat.report(b)
}

func benchGetRandom(b *testing.B, getFn func(string) ([]byte, error), setBatchFn func([]lsm.KVPair) error, n int) {
	prepopulate(setBatchFn, n)
	keys := make([]string, n)
	for i := 0; i < n; i++ {
		keys[i] = keySeq(i)
	}
	rng := rand.New(rand.NewSource(42))
	lat := &latencies{samples: make([]time.Duration, 0, b.N)}
	b.SetBytes(10)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := keys[rng.Intn(n)]
		lat.time(func() {
			if _, err := getFn(key); err != nil {
				b.Fatal(err)
			}
		})
	}
	lat.report(b)
}

func benchScan(b *testing.B, setBatchFn func([]lsm.KVPair) error, scanFn func(string, string) error, n int) {
	prepopulate(setBatchFn, n)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := keySeq(i % (n - 1000))
		end := keySeq((i % (n - 1000)) + 1000)
		if err := scanFn(start, end); err != nil {
			b.Fatal(err)
		}
	}
}

func benchCompact(b *testing.B, setBatchFn func([]lsm.KVPair) error, compactFn func() error, n int) {
	prepopulate(setBatchFn, n)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := compactFn(); err != nil {
			b.Fatal(err)
		}
	}
}

// WAL replay: build a log of n entries once, then measure full recovery
// (replay + index rebuild) via engine.Open.
func benchWALReplay(b *testing.B, n int) {
	dir, err := os.MkdirTemp(benchDir, "replay-*")
	if err != nil {
		b.Fatal(err)
	}
	defer os.RemoveAll(dir)

	store, err := engine.Open(dir)
	if err != nil {
		b.Fatal(err)
	}
	prepopulate(func(pairs []lsm.KVPair) error {
		converted := make([]engine.KVPair, len(pairs))
		for i, p := range pairs {
			converted[i] = engine.KVPair{Key: p.Key, Value: p.Value}
		}
		return store.SetBatch(converted)
	}, n)
	if err := store.Close(); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s, err := engine.Open(dir)
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		s.Close()
		b.StartTimer()
	}
	b.ReportMetric(float64(n)/(b.Elapsed().Seconds()/float64(b.N)), "entries/s")
}

func BenchmarkWALReplay_10k(b *testing.B)  { benchWALReplay(b, 10_000) }
func BenchmarkWALReplay_100k(b *testing.B) { benchWALReplay(b, 100_000) }
func BenchmarkWALReplay_1M(b *testing.B)   { benchWALReplay(b, 1_000_000) }

// B-tree benchmarks

func BenchmarkBTree_SetSequential(b *testing.B) {
	benchSetSequential(b, btreeStore.Set)
}

func BenchmarkBTree_SetRandom(b *testing.B) {
	benchSetRandom(b, btreeStore.Set)
}

func BenchmarkBTree_SetBatch100(b *testing.B) {
	benchSetBatch(b, btreeSetBatch, 100)
}

func BenchmarkBTree_GetSequential(b *testing.B) {
	benchGetSequential(b, btreeStore.Get, btreeSetBatch, benchPrepopGet)
}

func BenchmarkBTree_GetRandom(b *testing.B) {
	benchGetRandom(b, btreeStore.Get, btreeSetBatch, benchPrepopGet)
}

func BenchmarkBTree_Scan(b *testing.B) {
	benchScan(b, btreeSetBatch, func(start, end string) error {
		_, err := btreeStore.Scan(start, end)
		return err
	}, benchPrepopScan)
}

func BenchmarkBTree_Compact(b *testing.B) {
	benchCompact(b, btreeSetBatch, btreeStore.Compact, benchPrepopCompact)
}

// LSM benchmarks

func BenchmarkLSM_SetSequential(b *testing.B) {
	benchSetSequential(b, lsmStore.Set)
}

func BenchmarkLSM_SetRandom(b *testing.B) {
	benchSetRandom(b, lsmStore.Set)
}

func BenchmarkLSM_SetBatch100(b *testing.B) {
	benchSetBatch(b, lsmStore.SetBatch, 100)
}

func BenchmarkLSM_GetSequential(b *testing.B) {
	benchGetSequential(b, lsmStore.Get, lsmStore.SetBatch, benchPrepopGet)
}

func BenchmarkLSM_GetRandom(b *testing.B) {
	benchGetRandom(b, lsmStore.Get, lsmStore.SetBatch, benchPrepopGet)
}

func BenchmarkLSM_Scan(b *testing.B) {
	benchScan(b, lsmStore.SetBatch, func(start, end string) error {
		_, err := lsmStore.Scan(start, end)
		return err
	}, benchPrepopScan)
}

func BenchmarkLSM_Compact(b *testing.B) {
	benchCompact(b, lsmStore.SetBatch, lsmStore.Compact, benchPrepopCompact)
}
