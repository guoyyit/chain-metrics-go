// Package store persists blocks, published snapshots, checkpoints and
// the canonical head pointer on the local filesystem.
//
// Layout under the data directory:
//
//	blocks/<height>/<hash>.json   content-addressed blocks, never rewritten
//	snapshots.jsonl               append-only published snapshots
//	checkpoint.tmp/checkpoint.json   atomically rotated state checkpoint
//	head.tmp/head.json               atomically rotated canonical pointer
//
// All writes are either new content-addressed files or atomic
// temp-file+rename+fsync sequences, so a crash never leaves torn
// metadata.
package store

import (
	"io"
	"os"
	"path/filepath"

	"github.com/guoyyit/chain-metrics-go/domain"
	"github.com/guoyyit/chain-metrics-go/metrics"
)

// Store is the durable layer.
type Store interface {
	HasBlock(hash string) bool
	HasBlockAt(height uint64, hash string) bool
	SaveBlock(b *domain.Block) error
	LoadBlock(hash string) (*domain.Block, error)
	LoadBlockAt(height uint64, hash string) (*domain.Block, error)

	SaveSnapshot(snap *domain.BlockSnapshot) error
	SnapshotAtHeight(height uint64) (*domain.BlockSnapshot, error)
	PruneSnapshots(belowHeight uint64) error
	TruncateSnapshotsAbove(height uint64) error

	SaveReorgIntent(tip domain.HeadPointer) error
	LoadReorgIntent() (domain.HeadPointer, error)
	ClearReorgIntent() error
	RewriteArchiveDuringReorg(keepThrough uint64, branch []*domain.BlockSnapshot) error

	SaveCheckpoint(cp *metrics.Checkpoint) error
	LatestCheckpoint() (*metrics.Checkpoint, error)

	SaveHead(head domain.HeadPointer) error
	LoadHead() (domain.HeadPointer, error)

	// StreamBlocks enumerates every stored block exactly once. Order
	// is unspecified; full replay sorts by parent linkage.
	StreamBlocks(fn func(*domain.Block) error) error

	io.Closer
}

const (
	blocksDirName     = "blocks"
	snapshotsFileName = "snapshots.jsonl"
	checkpointName    = "checkpoint.json"
	checkpointTmpName = "checkpoint.tmp"
	headName          = "head.json"
	headTmpName       = "head.tmp"
)

// FileStore is the filesystem-backed Store.
type FileStore struct {
	dir         string
	blocksDir   string
	snapPath    string
	cpPath      string
	cpTmpPath   string
	headPath    string
	headTmpPath string
}

// Open creates/opens a FileStore rooted at dir.
func Open(dir string) (*FileStore, error) {
	if dir == "" {
		dir = "./metrics-data"
	}
	if err := os.MkdirAll(filepath.Join(dir, blocksDirName), 0o755); err != nil {
		return nil, err
	}
	s := &FileStore{
		dir:         dir,
		blocksDir:   filepath.Join(dir, blocksDirName),
		snapPath:    filepath.Join(dir, snapshotsFileName),
		cpPath:      filepath.Join(dir, checkpointName),
		cpTmpPath:   filepath.Join(dir, checkpointTmpName),
		headPath:    filepath.Join(dir, headName),
		headTmpPath: filepath.Join(dir, headTmpName),
	}
	return s, nil
}

func (s *FileStore) Close() error { return nil }

// Path returns the data directory (used in tests/docs).
func (s *FileStore) Path() string { return s.dir }
