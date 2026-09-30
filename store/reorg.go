package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"

	"github.com/guoyyit/chain-metrics-go/domain"
)

const (
	reorgIntentName = "reorg.tmp"
)

func (s *FileStore) reorgIntentPath() string { return s.dir + "/" + reorgIntentName }

// SaveReorgIntent durably records the new tip hash a reorg is
// switching to, before any archive or head mutation.
func (s *FileStore) SaveReorgIntent(tip domain.HeadPointer) error {
	data, err := json.Marshal(struct {
		Height uint64 `json:"h"`
		Hash   string `json:"hash"`
	}{Height: tip.Height, Hash: tip.Hash})
	if err != nil {
		return err
	}
	return atomicWrite(s.reorgIntentPath(), s.reorgIntentPath()+".tmp", data, 0o644)
}

// LoadReorgIntent reads an unfinished reorg intent.
func (s *FileStore) LoadReorgIntent() (domain.HeadPointer, error) {
	data, err := os.ReadFile(s.reorgIntentPath())
	if err != nil {
		if os.IsNotExist(err) {
			return domain.HeadPointer{}, domain.ErrNotFound
		}
		return domain.HeadPointer{}, err
	}
	var dto struct {
		Height uint64 `json:"h"`
		Hash   string `json:"hash"`
	}
	if err := json.Unmarshal(data, &dto); err != nil {
		return domain.HeadPointer{}, err
	}
	return domain.HeadPointer{Height: dto.Height, Hash: dto.Hash}, nil
}

// ClearReorgIntent removes a completed intent.
func (s *FileStore) ClearReorgIntent() error {
	err := os.Remove(s.reorgIntentPath())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// RewriteArchiveDuringReorg builds a fresh snapshot archive: existing
// lines at height <= keepThrough are retained, everything above is
// dropped and branch snapshots (ordered ascending) are appended. The
// rewrite is temp-file + fsync + rename, so the archive is either the
// old full version or the new one, never a mix.
func (s *FileStore) RewriteArchiveDuringReorg(keepThrough uint64, branch []*domain.BlockSnapshot) error {
	in, err := os.Open(s.snapPath)
	tmpPath := s.snapPath + ".reorg.tmp"
	out, oerr := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if oerr != nil {
		if in != nil {
			in.Close()
		}
		return oerr
	}

	writeLine := func(line []byte) error {
		_, err := out.Write(append(bytes.TrimRight(line, "\n"), '\n'))
		return err
	}

	if err == nil {
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
				in.Close()
				return err
			}
			if head.H <= keepThrough {
				if err := writeLine(line); err != nil {
					out.Close()
					in.Close()
					return err
				}
			}
		}
		if err := scanner.Err(); err != nil {
			out.Close()
			in.Close()
			return err
		}
		in.Close()
	}

	for _, snap := range branch {
		data, err := encodeSnapshot(snap)
		if err != nil {
			out.Close()
			return err
		}
		if _, err := out.Write(append(data, '\n')); err != nil {
			out.Close()
			return err
		}
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.snapPath); err != nil {
		return err
	}
	return syncDir(s.snapPath)
}
