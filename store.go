package chainmetrics

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
)

const (
	walName        = "blocks.log"
	headName       = "head.json"
	checkpointFile = "checkpoint.json"
)

// persistedBlock is the durable encoding of a canonical block.
type persistedBlock struct {
	Number     uint64
	Hash       string
	ParentHash string
	Events     []persistedEvent
}

// persistedEvent is the durable encoding of one event.
type persistedEvent struct {
	TokenID string
	Type    EventType
	From    string
	To      string
	Amount  string
	Index   uint32
}

// headRecord points at the durable canonical tip.
type headRecord struct {
	Number uint64
	Hash   string
}

// Store persists canonical blocks and the chain head for crash recovery.
type Store struct {
	dir       string
	wal       *os.File
	walWriter *bufio.Writer
}

func OpenStore(dir string) (*Store, error) {
	if dir == "" {
		return &Store{}, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// Remove stale temp files left behind by a crash mid-rename. They are
	// never referenced by any committed state.
	if entries, err := os.ReadDir(dir); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, ".wal-") ||
				strings.HasPrefix(name, ".head-") ||
				strings.HasPrefix(name, ".checkpoint-") {
				_ = os.Remove(filepath.Join(dir, name))
			}
		}
	}
	wal, err := os.OpenFile(filepath.Join(dir, walName), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Store{
		dir:       dir,
		wal:       wal,
		walWriter: bufio.NewWriter(wal),
	}, nil
}

func (s *Store) Close() error {
	if s.wal == nil {
		return nil
	}
	if err := s.walWriter.Flush(); err != nil {
		return err
	}
	return s.wal.Close()
}

// persistBlock durable-records one canonical block (events + hash linkage).
func (s *Store) persistBlock(b Block) error {
	if s.dir == "" {
		return nil
	}
	pb := persistedBlock{
		Number:     b.Number,
		Hash:       b.Hash,
		ParentHash: b.ParentHash,
		Events:     make([]persistedEvent, 0, len(b.Events)),
	}
	for _, e := range b.Events {
		pb.Events = append(pb.Events, persistedEvent{
			TokenID: e.TokenID,
			Type:    e.Type,
			From:    e.From,
			To:      e.To,
			Amount:  e.Amount.String(),
			Index:   e.Index,
		})
	}
	raw, err := json.Marshal(pb)
	if err != nil {
		return err
	}
	if _, err := s.walWriter.Write(raw); err != nil {
		return err
	}
	if err := s.walWriter.WriteByte('\n'); err != nil {
		return err
	}
	return s.walWriter.Flush()
}

// walkChain returns the blocks reachable from headHash via parent links, in
// ascending height order. Unreachable WAL records (stale fork branches,
// uncommitted appends) are ignored; duplicate hashes collapse.
func (s *Store) walkChain(headHash string) ([]Block, error) {
	if s.dir == "" {
		return nil, nil
	}
	blocks, err := s.readWAL()
	if err != nil {
		return nil, err
	}
	byHash := make(map[string]*persistedBlock, len(blocks))
	for i := range blocks {
		byHash[blocks[i].Hash] = &blocks[i]
	}
	var ordered []*persistedBlock
	for node := byHash[headHash]; node != nil; node = byHash[node.ParentHash] {
		ordered = append(ordered, node)
	}
	out := make([]Block, 0, len(ordered))
	for i := len(ordered) - 1; i >= 0; i-- {
		b, err := ordered[i].toBlock()
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// compactWal atomically rewrites the WAL keeping only blocks that are
// reachable from headHash via parent links AND have height above
// aboveHeight. Blocks at or below aboveHeight are redundant because the
// durable checkpoint image at aboveHeight covers them; unreachable blocks
// belong to abandoned branches. Both the old and the new WAL are valid for
// the current head, so a crash mid-compaction is harmless.
func (s *Store) compactWal(headHash string, aboveHeight uint64) error {
	if s.dir == "" {
		return nil
	}
	blocks, err := s.readWAL()
	if err != nil {
		return err
	}
	byHash := make(map[string]*persistedBlock, len(blocks))
	for i := range blocks {
		byHash[blocks[i].Hash] = &blocks[i]
	}
	keep := make(map[string]bool)
	for node := byHash[headHash]; node != nil && node.Number > aboveHeight; node = byHash[node.ParentHash] {
		keep[node.Hash] = true
	}
	return s.rewriteWal(func(pb *persistedBlock) bool { return keep[pb.Hash] })
}

// rewriteWal atomically replaces the WAL with the records accepted by keep.
func (s *Store) rewriteWal(keep func(*persistedBlock) bool) error {
	if s.dir == "" {
		return nil
	}
	blocks, err := s.readWAL()
	if err != nil {
		return err
	}
	if err := s.walWriter.Flush(); err != nil {
		return err
	}
	if err := s.wal.Close(); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(s.dir, ".wal-*")
	if err != nil {
		return err
	}
	w := bufio.NewWriter(tmp)
	for _, pb := range blocks {
		if !keep(&pb) {
			continue
		}
		raw, err := json.Marshal(pb)
		if err != nil {
			tmp.Close()
			return err
		}
		if _, err := w.Write(raw); err != nil {
			tmp.Close()
			return err
		}
		if err := w.WriteByte('\n'); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(s.dir, walName)); err != nil {
		return err
	}
	s.wal, err = os.OpenFile(filepath.Join(s.dir, walName), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	s.walWriter = bufio.NewWriter(s.wal)
	return nil
}

// saveHead durably records the canonical tip via temp-file + atomic rename.
func (s *Store) saveHead(number uint64, hash string) error {
	if s.dir == "" {
		return nil
	}
	raw, err := json.Marshal(headRecord{Number: number, Hash: hash})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".head-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(s.dir, headName))
}

// loadHead returns the durable canonical tip; ok=false when never committed.
func (s *Store) loadHead() (number uint64, hash string, ok bool, err error) {
	if s.dir == "" {
		return 0, "", false, nil
	}
	raw, err := os.ReadFile(filepath.Join(s.dir, headName))
	if os.IsNotExist(err) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, err
	}
	var h headRecord
	if err := json.Unmarshal(raw, &h); err != nil {
		return 0, "", false, err
	}
	return h.Number, h.Hash, true, nil
}

// saveCheckpoint atomically writes a full state image.
func (s *Store) saveCheckpoint(raw []byte) error {
	if s.dir == "" {
		return nil
	}
	tmp, err := os.CreateTemp(s.dir, ".checkpoint-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(s.dir, checkpointFile))
}

// loadCheckpoint reads a persisted state image, if any.
func (s *Store) loadCheckpoint() ([]byte, error) {
	if s.dir == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(filepath.Join(s.dir, checkpointFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	return raw, err
}

// readWAL reads all durable block records; a torn trailing line is discarded.
func (s *Store) readWAL() ([]persistedBlock, error) {
	f, err := os.Open(filepath.Join(s.dir, walName))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var blocks []persistedBlock
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var pb persistedBlock
		if err := json.Unmarshal(line, &pb); err != nil {
			// A partial final line from a crash mid-append is discarded.
			break
		}
		blocks = append(blocks, pb)
	}
	return blocks, scanner.Err()
}

func (pb *persistedBlock) toBlock() (Block, error) {
	b := Block{Number: pb.Number, Hash: pb.Hash, ParentHash: pb.ParentHash}
	for _, pe := range pb.Events {
		amount, ok := new(big.Int).SetString(pe.Amount, 10)
		if !ok {
			return Block{}, fmt.Errorf("invalid amount %q", pe.Amount)
		}
		b.Events = append(b.Events, Event{
			TokenID: pe.TokenID,
			Type:    pe.Type,
			From:    pe.From,
			To:      pe.To,
			Amount:  amount,
			Block:   pb.Number,
			Index:   pe.Index,
		})
	}
	return b, nil
}
