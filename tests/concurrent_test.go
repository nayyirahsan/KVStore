package tests

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kvstore/engine"
)

func TestConcurrentReadsWithWrites(t *testing.T) {
	store, _ := openTestStore(t)
	defer store.Close()

	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("key%04d", i)
		require.NoError(t, store.Set(key, []byte(key)))
	}

	var wg sync.WaitGroup
	errs := make(chan error, 100)

	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				key := fmt.Sprintf("key%04d", i%100)
				if _, err := store.Get(key); err != nil {
					errs <- err
				}
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		assert.NoError(t, err)
	}
}

// Full mixed workload under the race detector: concurrent readers, writers,
// deleters, scanners, and a compactor. Verifies no data races and that every
// value read is one some writer actually wrote for that key.
func TestConcurrentMixedWorkloadWithCompaction(t *testing.T) {
	store, _ := openTestStore(t)
	defer store.Close()

	const keySpace = 200
	key := func(i int) string { return fmt.Sprintf("key%04d", i%keySpace) }

	for i := 0; i < keySpace; i++ {
		require.NoError(t, store.Set(key(i), []byte(key(i)+":init")))
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 1000)

	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				k := key(id*31 + i)
				if err := store.Set(k, []byte(k+":w")); err != nil {
					errCh <- err
				}
			}
		}(w)
	}
	for d := 0; d < 2; d++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if err := store.Delete(key(id*17 + i*3)); err != nil {
					errCh <- err
				}
			}
		}(d)
	}
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				k := key(id*13 + i)
				val, err := store.Get(k)
				if err == engine.ErrKeyNotFound {
					continue // deleted concurrently; fine
				}
				if err != nil {
					errCh <- err
					continue
				}
				// Any observed value must be a legitimate write for this key.
				if string(val) != k+":init" && string(val) != k+":w" {
					errCh <- fmt.Errorf("key %s: torn/foreign value %q", k, val)
				}
			}
		}(r)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			if err := store.Compact(); err != nil {
				errCh <- err
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if _, err := store.Scan("key0000", "key0100"); err != nil {
				errCh <- err
			}
		}
	}()

	wg.Wait()
	close(errCh)
	for err := range errCh {
		assert.NoError(t, err)
	}
}
