package chainmetrics

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestHistoryOne exercises the tightest possible retention: HistoryBlocks=1
// means the state image follows the tip on every block, yet a depth-1 reorg
// must succeed, survive restart with per-block equality to a fresh replay,
// and a depth-2 reorg must be rejected.
func TestHistoryOne(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.HistoryBlocks = 1
	canonical := deepChain(10, "h")
	fork := forkChain(canonical[8], 9, 2, "r")
	newChain := append(append([]Block{}, canonical[:9]...), fork...)
	ref, _ := RebuildFromScratch(newChain, cfg.Metrics)

	func() {
		e, err := NewEngine(cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range canonical {
			mustPush(t, e, b)
		}
		for _, b := range fork {
			mustPush(t, e, b)
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	e2, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	for h := uint64(9); h <= 11; h++ {
		s, ok := e2.QueryAt(h)
		if !ok || !snapshotsEqual(s, ref[h]) {
			t.Fatalf("R=1 reorg restart mismatch at %d ok=%v", h, ok)
		}
	}

	deep := forkChain(canonical[7], 8, 4, "d")
	var got error
	for _, b := range deep {
		if er := e2.PushBlock(b); er != nil {
			got = er
		}
	}
	if !errors.Is(got, ErrReorgTooDeep) {
		t.Fatalf("depth-2 reorg with R=1: want ErrReorgTooDeep, got %v", got)
	}
	if s, ok := e2.QueryLatest(); !ok || s.BlockHash != "r11" {
		t.Fatalf("rejected reorg moved the head to %s", s.BlockHash)
	}
}
