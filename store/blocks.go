package store

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/guoyyit/chain-metrics-go/domain"
)

// sanitizeHash makes an arbitrary block hash safe as a filename.
func sanitizeHash(hash string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", "..", "__")
	return r.Replace(hash)
}

func (s *FileStore) blockPath(b *domain.Block) string {
	return filepath.Join(s.blocksDir,
		uintToString(b.Height), sanitizeHash(b.Hash)+".json")
}

func (s *FileStore) blockPathFor(height uint64, hash string) string {
	return filepath.Join(s.blocksDir,
		uintToString(height), sanitizeHash(hash)+".json")
}

// statBlockAt is the O(1) existence check when the caller knows both
// the height and hash (the common ingest and chain-walk path).
func (s *FileStore) statBlockAt(height uint64, hash string) bool {
	_, err := os.Stat(s.blockPathFor(height, hash))
	return err == nil
}

// HasBlockAt reports whether a content-addressed block exists at the
// sharded (height, hash) path without scanning the directory.
func (s *FileStore) HasBlockAt(height uint64, hash string) bool {
	if hash == "" {
		return false
	}
	return s.statBlockAt(height, hash)
}

// HasBlock reports whether the content-addressed block is stored. It
// is the height-agnostic fallback and requires a directory walk.
func (s *FileStore) HasBlock(hash string) bool {
	if hash == "" {
		return false
	}
	found := false
	_ = filepath.WalkDir(s.blocksDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.TrimSuffix(d.Name(), ".json") == sanitizeHash(hash) {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// SaveBlock writes a block exactly once. Re-saving the same hash is a
// no-op, making duplicate delivery idempotent.
func (s *FileStore) SaveBlock(b *domain.Block) error {
	if b == nil || b.Hash == "" {
		return domain.ErrInvalidBlock
	}
	path := s.blockPath(b)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	data, err := encodeBlock(b)
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return atomicWriteNoDirfsync(path, path+".tmp", data, 0o644)
}

// LoadBlockAt is the height-aware O(1) fast path.
func (s *FileStore) LoadBlockAt(height uint64, hash string) (*domain.Block, error) {
	data, err := os.ReadFile(s.blockPathFor(height, hash))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	return decodeBlock(data)
}

// LoadBlock reads a block by hash. It tries the sharded path at the
// height encoded in the filename space is unknown to callers during
// chain walking, so a directory walk remains as the fallback.
func (s *FileStore) LoadBlock(hash string) (*domain.Block, error) {
	var found string
	_ = filepath.WalkDir(s.blocksDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.TrimSuffix(d.Name(), ".json") == sanitizeHash(hash) {
			found = path
			return fs.SkipAll
		}
		return nil
	})
	if found == "" {
		return nil, domain.ErrNotFound
	}
	data, err := os.ReadFile(found)
	if err != nil {
		return nil, err
	}
	return decodeBlock(data)
}

// StreamBlocks enumerates every stored block exactly once.
func (s *FileStore) StreamBlocks(fn func(*domain.Block) error) error {
	return filepath.WalkDir(s.blocksDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		b, err := decodeBlock(data)
		if err != nil {
			return err
		}
		return fn(b)
	})
}
