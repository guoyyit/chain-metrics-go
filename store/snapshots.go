package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/guoyyit/chain-metrics-go/domain"
	"github.com/guoyyit/chain-metrics-go/metrics"
)

// SaveSnapshot appends one published snapshot as one JSON line and
// fsyncs before returning. The head pointer write performed by the
// caller afterwards is the publication boundary: either both the
// snapshot line and the new head are durable, or the head still points
// at the previous block and the trailing line is re-derived on
// recovery.
func (s *FileStore) SaveSnapshot(snap *domain.BlockSnapshot) error {
	data, err := encodeSnapshot(snap)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.snapPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// SnapshotAtHeight scans the append-only archive for the exact height
// and returns ErrNotFound otherwise. It is a streaming scan, not a
// full history cache, so memory stays bounded.
func (s *FileStore) SnapshotAtHeight(height uint64) (*domain.BlockSnapshot, error) {
	f, err := os.Open(s.snapPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var head struct {
			H uint64 `json:"h"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			return nil, err
		}
		if head.H < height {
			continue
		}
		if head.H > height {
			return nil, domain.ErrNotFound
		}
		return decodeSnapshot(line)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return nil, domain.ErrNotFound
}

// PruneSnapshots rewrites the archive keeping only heights >= floor.
// A temp file is built, fsynced and atomically renamed.
func (s *FileStore) PruneSnapshots(belowHeight uint64) error {
	in, err := os.Open(s.snapPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	tmpPath := s.snapPath + ".prune.tmp"
	out, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		in.Close()
		return err
	}

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	keep := false
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var head struct {
			H uint64 `json:"h"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			out.Close()
			in.Close()
			return err
		}
		if head.H >= belowHeight {
			if _, err := out.Write(append(line, '\n')); err != nil {
				out.Close()
				in.Close()
				return err
			}
			keep = true
		}
	}
	scanErr := scanner.Err()
	if err := out.Sync(); err != nil {
		out.Close()
		in.Close()
		return err
	}
	if err := out.Close(); err != nil {
		in.Close()
		return err
	}
	if err := in.Close(); err != nil {
		return err
	}
	if scanErr != nil {
		return scanErr
	}
	if !keep {
		// Nothing retained: remove temp so an empty archive results.
		if err := os.Rename(tmpPath, s.snapPath); err != nil {
			return err
		}
	} else if err := os.Rename(tmpPath, s.snapPath); err != nil {
		return err
	}
	return syncDir(s.snapPath)
}

// TruncateSnapshotsAbove rewrites the archive dropping heights above
// the given height. It is the rollback path when a snapshot line was
// appended but the head pointer commit failed.
func (s *FileStore) TruncateSnapshotsAbove(height uint64) error {
	in, err := os.Open(s.snapPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer in.Close()

	tmpPath := s.snapPath + ".truncate.tmp"
	out, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var head struct {
			H uint64 `json:"h"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			out.Close()
			return err
		}
		if head.H <= height {
			if _, err := out.Write(append(line, '\n')); err != nil {
				out.Close()
				return err
			}
		}
	}
	scanErr := scanner.Err()
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if scanErr != nil {
		return scanErr
	}
	if err := os.Rename(tmpPath, s.snapPath); err != nil {
		return err
	}
	return syncDir(s.snapPath)
}

// SaveCheckpoint atomically rotates the state checkpoint.
func (s *FileStore) SaveCheckpoint(cp *metrics.Checkpoint) error {
	data, err := json.Marshal(struct {
		Height uint64 `json:"h"`
		Hash   string `json:"hash"`
		Data   []byte `json:"d"`
	}{Height: cp.Height, Hash: cp.Hash, Data: cp.Data})
	if err != nil {
		return err
	}
	return atomicWrite(s.cpPath, s.cpTmpPath, data, 0o644)
}

// LatestCheckpoint returns the most recent checkpoint, or ErrNotFound.
func (s *FileStore) LatestCheckpoint() (*metrics.Checkpoint, error) {
	data, err := os.ReadFile(s.cpPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	var dto struct {
		Height uint64 `json:"h"`
		Hash   string `json:"hash"`
		Data   []byte `json:"d"`
	}
	if err := json.Unmarshal(data, &dto); err != nil {
		return nil, err
	}
	return &metrics.Checkpoint{Height: dto.Height, Hash: dto.Hash, Data: dto.Data}, nil
}

type headDTO struct {
	Height uint64 `json:"h"`
	Hash   string `json:"hash"`
}

// SaveHead atomically publishes the canonical head pointer.
func (s *FileStore) SaveHead(head domain.HeadPointer) error {
	data, err := json.Marshal(headDTO{Height: head.Height, Hash: head.Hash})
	if err != nil {
		return err
	}
	return atomicWrite(s.headPath, s.headTmpPath, data, 0o644)
}

// LoadHead reads the canonical pointer. Returns ErrNotFound before the
// first block has been published.
func (s *FileStore) LoadHead() (domain.HeadPointer, error) {
	data, err := os.ReadFile(s.headPath)
	if err != nil {
		if os.IsNotExist(err) {
			return domain.HeadPointer{}, domain.ErrNotFound
		}
		return domain.HeadPointer{}, err
	}
	var dto headDTO
	if err := json.Unmarshal(data, &dto); err != nil {
		return domain.HeadPointer{}, err
	}
	return domain.HeadPointer{Height: dto.Height, Hash: dto.Hash}, nil
}
