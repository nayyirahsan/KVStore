package lsm

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const manifestFile = "manifest"

// syncDir fsyncs a directory so a preceding rename in it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

type manifestSST struct {
	level int
	path  string
	seq   uint64
}

type manifestData struct {
	seq    uint64
	tables []manifestSST
}

func (s *Store) writeManifest() error {
	tmpPath := filepath.Join(s.dir, manifestFile+".tmp")
	finalPath := filepath.Join(s.dir, manifestFile)

	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	w := bufio.NewWriter(f)
	fmt.Fprintf(w, "MANIFEST v1\n")
	fmt.Fprintf(w, "SEQ\t%d\n", s.seq)
	for level, tables := range s.levels {
		for _, sst := range tables {
			// Min/max keys are hex-encoded: raw keys containing tabs or
			// newlines would corrupt this line-based format.
			fmt.Fprintf(w, "SST\t%d\t%s\t%s\t%s\t%d\n",
				level, hex.EncodeToString([]byte(sst.MinKey())),
				hex.EncodeToString([]byte(sst.MaxKey())),
				filepath.Base(sst.Path()), sst.Seq())
		}
	}
	fmt.Fprintf(w, "END\n")

	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return err
	}
	return syncDir(s.dir)
}

func loadManifest(dir string) (manifestData, error) {
	path := filepath.Join(dir, manifestFile)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return manifestData{}, nil
		}
		return manifestData{}, err
	}
	defer f.Close()

	data := manifestData{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "MANIFEST v1" || line == "END" || line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		switch parts[0] {
		case "SEQ":
			if len(parts) < 2 {
				return manifestData{}, fmt.Errorf("malformed manifest SEQ line: %q", line)
			}
			data.seq, _ = strconv.ParseUint(parts[1], 10, 64)
		case "SST":
			if len(parts) < 6 {
				return manifestData{}, fmt.Errorf("malformed manifest SST line: %q", line)
			}
			level, _ := strconv.Atoi(parts[1])
			seq, _ := strconv.ParseUint(parts[5], 10, 64)
			data.tables = append(data.tables, manifestSST{
				level: level,
				path:  parts[4],
				seq:   seq,
			})
		}
	}
	return data, scanner.Err()
}
