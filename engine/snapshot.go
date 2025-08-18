package engine

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// v2: keys are hex-encoded alongside values, so keys containing tabs,
	// newlines, or arbitrary bytes cannot corrupt the line-based format.
	snapshotHeader = "SNAPSHOT v2\n"
	snapshotFile   = "snapshot"
	snapshotTemp   = "snapshot.tmp"
)

// syncDir fsyncs a directory so a preceding rename in it is durable.
// Without this, a crash after rename(2) can roll the directory entry back
// to the pre-rename state on some filesystems.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

type Snapshot struct {
	dir string
}

func (s *Snapshot) Write(tree *BTree) error {
	tmpPath := filepath.Join(s.dir, snapshotTemp)
	finalPath := filepath.Join(s.dir, snapshotFile)

	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("create snapshot temp: %w", err)
	}

	var checksumData []byte
	writeLine := func(line string) error {
		checksumData = append(checksumData, []byte(line)...)
		_, err := f.WriteString(line)
		return err
	}

	if err := writeLine(snapshotHeader); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write snapshot header: %w", err)
	}

	pairs := tree.Scan("", "") // empty end = unbounded: every key, no sentinel
	for _, pair := range pairs {
		line := hex.EncodeToString([]byte(pair.Key)) + "\t" + hex.EncodeToString(pair.Value) + "\n"
		if err := writeLine(line); err != nil {
			f.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("write snapshot entry: %w", err)
		}
	}

	checksum := crc32.ChecksumIEEE(checksumData)
	checksumLine := fmt.Sprintf("CHECKSUM %d\n", checksum)
	if _, err := f.WriteString(checksumLine); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write snapshot checksum: %w", err)
	}

	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("snapshot fsync: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("snapshot close: %w", err)
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		return fmt.Errorf("snapshot rename: %w", err)
	}
	if err := syncDir(s.dir); err != nil {
		return fmt.Errorf("snapshot dir fsync: %w", err)
	}
	return nil
}

func (s *Snapshot) Load() (*BTree, error) {
	path := filepath.Join(s.dir, snapshotFile)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewBTree(), nil
		}
		return nil, fmt.Errorf("open snapshot: %w", err)
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read snapshot: %w", err)
	}
	if len(lines) == 0 {
		return NewBTree(), nil
	}

	if lines[0] != strings.TrimSuffix(snapshotHeader, "\n") {
		return nil, fmt.Errorf("invalid snapshot header")
	}

	if len(lines) < 2 {
		return nil, fmt.Errorf("snapshot missing checksum")
	}

	checksumLine := lines[len(lines)-1]
	dataLines := lines[1 : len(lines)-1]

	if !strings.HasPrefix(checksumLine, "CHECKSUM ") {
		return nil, fmt.Errorf("invalid checksum line")
	}
	expectedChecksum, err := strconv.ParseUint(strings.TrimPrefix(checksumLine, "CHECKSUM "), 10, 32)
	if err != nil {
		return nil, fmt.Errorf("parse checksum: %w", err)
	}

	var checksumData []byte
	checksumData = append(checksumData, []byte(lines[0])...)
	checksumData = append(checksumData, '\n')
	for _, line := range dataLines {
		checksumData = append(checksumData, []byte(line)...)
		checksumData = append(checksumData, '\n')
	}
	computed := crc32.ChecksumIEEE(checksumData)
	if uint32(expectedChecksum) != computed {
		return nil, fmt.Errorf("snapshot checksum mismatch")
	}

	tree := NewBTree()
	for _, line := range dataLines {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid snapshot line: %q", line)
		}
		key, err := hex.DecodeString(parts[0])
		if err != nil {
			return nil, fmt.Errorf("decode key %q: %w", parts[0], err)
		}
		value, err := hex.DecodeString(parts[1])
		if err != nil {
			return nil, fmt.Errorf("decode value for key %q: %w", parts[0], err)
		}
		tree.Set(string(key), value)
	}

	return tree, nil
}
