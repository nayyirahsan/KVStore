package engine

import (
	"fmt"
	"path/filepath"
)

const walFile = "wal"

func Recover(dir string) (*BTree, *WAL, error) {
	snap := &Snapshot{dir: dir}
	tree, err := snap.Load()
	if err != nil {
		return nil, nil, fmt.Errorf("load snapshot: %w", err)
	}

	walPath := filepath.Join(dir, walFile)
	wal, err := OpenWAL(walPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open wal: %w", err)
	}

	entries, err := wal.Replay()
	if err != nil {
		wal.Close()
		return nil, nil, fmt.Errorf("replay wal: %w", err)
	}

	for _, entry := range entries {
		switch entry.Type {
		case TypeSet:
			tree.Set(entry.Key, entry.Value)
		case TypeDelete:
			tree.Delete(entry.Key)
		default:
			wal.Close()
			return nil, nil, fmt.Errorf("unknown wal entry type: %d", entry.Type)
		}
	}

	return tree, wal, nil
}
