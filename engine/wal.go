package engine

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"os"
)

const (
	TypeSet    byte = 0x01
	TypeDelete byte = 0x02
)

type WALEntry struct {
	Type  byte
	Key   string
	Value []byte
}

type WAL struct {
	file *os.File
	path string
}

func OpenWAL(path string) (*WAL, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("open wal: %w", err)
	}
	return &WAL{file: f, path: path}, nil
}

func (w *WAL) Append(entry WALEntry) error {
	rec, err := encodeRecord(entry)
	if err != nil {
		return err
	}
	if _, err := w.file.Write(rec); err != nil {
		return fmt.Errorf("wal write: %w", err)
	}
	if err := w.file.Sync(); err != nil {
		return fmt.Errorf("wal fsync: %w", err)
	}
	return nil
}

// AppendBatch writes all entries with a single fsync (group commit). The
// batch is not atomic — a crash can persist only a prefix of it — but every
// record carries its own length+CRC header, so recovery keeps exactly the
// intact prefix and truncates the rest.
func (w *WAL) AppendBatch(entries []WALEntry) error {
	if len(entries) == 0 {
		return nil
	}
	var buf []byte
	for _, e := range entries {
		rec, err := encodeRecord(e)
		if err != nil {
			return err
		}
		buf = append(buf, rec...)
	}
	if _, err := w.file.Write(buf); err != nil {
		return fmt.Errorf("wal batch write: %w", err)
	}
	if err := w.file.Sync(); err != nil {
		return fmt.Errorf("wal fsync: %w", err)
	}
	return nil
}

// encodeRecord frames a serialized entry as [len u32][crc u32][payload].
func encodeRecord(entry WALEntry) ([]byte, error) {
	payload, err := serializeEntry(entry)
	if err != nil {
		return nil, err
	}
	rec := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(rec[0:4], uint32(len(payload)))
	binary.BigEndian.PutUint32(rec[4:8], crc32.ChecksumIEEE(payload))
	copy(rec[8:], payload)
	return rec, nil
}

func serializeEntry(entry WALEntry) ([]byte, error) {
	keyBytes := []byte(entry.Key)
	valueLen := len(entry.Value)
	if entry.Type == TypeDelete {
		valueLen = 0
	}

	total := 1 + 4 + 4 + len(keyBytes) + valueLen
	buf := make([]byte, total)
	buf[0] = entry.Type
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(keyBytes)))
	binary.BigEndian.PutUint32(buf[5:9], uint32(valueLen))
	copy(buf[9:], keyBytes)
	if entry.Type == TypeSet {
		copy(buf[9+len(keyBytes):], entry.Value)
	}
	return buf, nil
}

// Replay reads every intact entry from the start of the log. If it finds a
// torn or corrupt tail (crash mid-append), it discards the tail AND truncates
// the file back to the last intact entry. The truncation is load-bearing:
// the WAL is opened with O_APPEND, so without it, post-recovery appends would
// land after the garbage bytes and every future replay would stop at the
// garbage and lose them.
func (w *WAL) Replay() ([]WALEntry, error) {
	if _, err := w.file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("wal seek: %w", err)
	}
	info, err := w.file.Stat()
	if err != nil {
		return nil, fmt.Errorf("wal stat: %w", err)
	}
	fileSize := info.Size()

	var entries []WALEntry
	var offset int64 // end of the last intact entry

	for {
		var header [8]byte
		n, err := io.ReadFull(w.file, header[:])
		if err == io.EOF {
			break
		}
		if err == io.ErrUnexpectedEOF || n < 8 {
			log.Printf("WAL truncated at offset %d due to torn header", offset)
			break
		}
		if err != nil {
			return nil, fmt.Errorf("wal read header at offset %d: %w", offset, err)
		}

		length := binary.BigEndian.Uint32(header[0:4])
		storedCRC := binary.BigEndian.Uint32(header[4:8])

		// A torn header can decode to an arbitrary length; bounds-check against
		// the file size before allocating or reading.
		if offset+8+int64(length) > fileSize {
			log.Printf("WAL truncated at offset %d due to incomplete entry", offset)
			break
		}

		payload := make([]byte, length)
		if _, err := io.ReadFull(w.file, payload); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				log.Printf("WAL truncated at offset %d due to incomplete entry", offset)
				break
			}
			return nil, fmt.Errorf("wal read payload at offset %d: %w", offset, err)
		}

		computedCRC := crc32.ChecksumIEEE(payload)
		if computedCRC != storedCRC {
			log.Printf("WAL truncated at offset %d due to checksum mismatch", offset)
			break
		}

		entry, err := parseEntry(payload)
		if err != nil {
			log.Printf("WAL truncated at offset %d due to parse error: %v", offset, err)
			break
		}
		entries = append(entries, entry)
		offset += int64(8 + length)
	}

	if offset < fileSize {
		if err := w.file.Truncate(offset); err != nil {
			return nil, fmt.Errorf("wal truncate torn tail at offset %d: %w", offset, err)
		}
		if err := w.file.Sync(); err != nil {
			return nil, fmt.Errorf("wal fsync after tail truncate: %w", err)
		}
	}

	return entries, nil
}

func parseEntry(payload []byte) (WALEntry, error) {
	if len(payload) < 9 {
		return WALEntry{}, fmt.Errorf("payload too short")
	}
	entryType := payload[0]
	keyLen := binary.BigEndian.Uint32(payload[1:5])
	valueLen := binary.BigEndian.Uint32(payload[5:9])

	if int(9+keyLen+valueLen) != len(payload) {
		return WALEntry{}, fmt.Errorf("invalid payload lengths")
	}

	key := string(payload[9 : 9+keyLen])
	var value []byte
	if valueLen > 0 {
		value = make([]byte, valueLen)
		copy(value, payload[9+keyLen:])
	}

	return WALEntry{Type: entryType, Key: key, Value: value}, nil
}

func (w *WAL) Truncate() error {
	if err := w.file.Truncate(0); err != nil {
		return fmt.Errorf("wal truncate: %w", err)
	}
	if _, err := w.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("wal seek after truncate: %w", err)
	}
	if err := w.file.Sync(); err != nil {
		return fmt.Errorf("wal fsync after truncate: %w", err)
	}
	return nil
}

func (w *WAL) Close() error {
	return w.file.Close()
}

func (w *WAL) Path() string {
	return w.path
}
