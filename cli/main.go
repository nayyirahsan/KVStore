package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"kvstore/engine"
	"kvstore/lsm"
)

var (
	dir    = flag.String("dir", "./kvstore-data", "storage directory")
	engineFlag = flag.String("engine", "btree", "storage engine: btree or lsm")
)

type kvStore interface {
	Get(key string) ([]byte, error)
	Set(key string, value []byte) error
	Delete(key string) error
	Scan(start, end string) ([]engine.KVPair, error)
	Compact() error
	Close() error
}

type lsmAdapter struct {
	s *lsm.Store
}

func (a *lsmAdapter) Get(key string) ([]byte, error)             { return a.s.Get(key) }
func (a *lsmAdapter) Set(key string, value []byte) error         { return a.s.Set(key, value) }
func (a *lsmAdapter) Delete(key string) error                    { return a.s.Delete(key) }
func (a *lsmAdapter) Compact() error                             { return a.s.Compact() }
func (a *lsmAdapter) Close() error                               { return a.s.Close() }
func (a *lsmAdapter) Scan(start, end string) ([]engine.KVPair, error) {
	pairs, err := a.s.Scan(start, end)
	if err != nil {
		return nil, err
	}
	result := make([]engine.KVPair, len(pairs))
	for i, p := range pairs {
		result[i] = engine.KVPair{Key: p.Key, Value: p.Value}
	}
	return result, nil
}

func openStore() (kvStore, error) {
	switch *engineFlag {
	case "btree":
		return engine.Open(*dir)
	case "lsm":
		s, err := lsm.Open(*dir)
		if err != nil {
			return nil, err
		}
		return &lsmAdapter{s: s}, nil
	default:
		return nil, fmt.Errorf("unknown engine: %s", *engineFlag)
	}
}

func main() {
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		printUsage()
		os.Exit(1)
	}

	store, err := openStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "open store: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	cmd := args[0]
	switch cmd {
	case "set":
		if len(args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: kvstore set <key> <value>")
			os.Exit(1)
		}
		if err := store.Set(args[1], []byte(args[2])); err != nil {
			fmt.Fprintf(os.Stderr, "set: %v\n", err)
			os.Exit(1)
		}
	case "get":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: kvstore get <key>")
			os.Exit(1)
		}
		val, err := store.Get(args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "get: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(string(val))
	case "delete":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: kvstore delete <key>")
			os.Exit(1)
		}
		if err := store.Delete(args[1]); err != nil {
			fmt.Fprintf(os.Stderr, "delete: %v\n", err)
			os.Exit(1)
		}
	case "scan":
		if len(args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: kvstore scan <start> <end>")
			os.Exit(1)
		}
		pairs, err := store.Scan(args[1], args[2])
		if err != nil {
			fmt.Fprintf(os.Stderr, "scan: %v\n", err)
			os.Exit(1)
		}
		for _, p := range pairs {
			fmt.Printf("%s\t%s\n", p.Key, string(p.Value))
		}
	case "compact":
		if err := store.Compact(); err != nil {
			fmt.Fprintf(os.Stderr, "compact: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("compaction complete")
	case "bench":
		ops := 100000
		for i := 1; i < len(args); i++ {
			if args[i] == "--ops" && i+1 < len(args) {
				ops, _ = strconv.Atoi(args[i+1])
			}
		}
		runBench(store, ops)
	default:
		printUsage()
		os.Exit(1)
	}
}

func runBench(store kvStore, ops int) {
	value := []byte("bench-value")

	start := time.Now()
	for i := 0; i < ops; i++ {
		key := fmt.Sprintf("bench%08d", i)
		if err := store.Set(key, value); err != nil {
			fmt.Fprintf(os.Stderr, "bench set: %v\n", err)
			os.Exit(1)
		}
	}
	writeDur := time.Since(start)
	writeOps := float64(ops) / writeDur.Seconds()
	fmt.Printf("Sequential writes: %.0f ops/sec (%.2f µs/op)\n", writeOps, float64(writeDur.Microseconds())/float64(ops))

	start = time.Now()
	for i := 0; i < ops; i++ {
		key := fmt.Sprintf("bench%08d", i%ops)
		if _, err := store.Get(key); err != nil {
			fmt.Fprintf(os.Stderr, "bench get: %v\n", err)
			os.Exit(1)
		}
	}
	readDur := time.Since(start)
	readOps := float64(ops) / readDur.Seconds()
	fmt.Printf("Random reads:      %.0f ops/sec (%.2f µs/op)\n", readOps, float64(readDur.Microseconds())/float64(ops))

	if err := store.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "close: %v\n", err)
		os.Exit(1)
	}

	start = time.Now()
	s, err := openStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "reopen: %v\n", err)
		os.Exit(1)
	}
	for i := 0; i < ops; i++ {
		key := fmt.Sprintf("bench%08d", i)
		if _, err := s.Get(key); err != nil {
			fmt.Fprintf(os.Stderr, "recovery get: %v\n", err)
			os.Exit(1)
		}
	}
	s.Close()
	recoverDur := time.Since(start)
	fmt.Printf("Crash recovery:    recovered %d keys in %.2fs\n", ops, recoverDur.Seconds())
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage:
  kvstore -dir ./data -engine btree|lsm set <key> <value>
  kvstore -dir ./data get <key>
  kvstore -dir ./data delete <key>
  kvstore -dir ./data scan <start> <end>
  kvstore -dir ./data compact
  kvstore -dir ./data bench --ops 100000
`)
}
