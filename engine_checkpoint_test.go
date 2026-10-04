package chainmetrics

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckpointRecovery(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.HistoryBlocks = 5

	const token = "TKN"
	makeBlocks := func(n uint64) []Block {
		var blocks []Block
		for i := uint64(1); i <= n; i++ {
			parent := ""
			if i > 1 {
				parent = "h" + uintToStr(i-1)
			}
			events := []Event{ev(token, Mint, "", "A", 1, i, 0)}
			if i > 1 {
				events = append(events,
					ev(token, Transfer, "A", "B", 1, i, 1),
					ev(token, Transfer, "B", "A", 1, i, 2))
			}
			blocks = append(blocks, block(i, "h"+uintToStr(i), parent, events...))
		}
		return blocks
	}

	blocks := makeBlocks(50)

	func() {
		e, err := NewEngine(cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range blocks {
			mustPush(t, e, b)
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	// The WAL retains only blocks above the latest checkpoint image, i.e. at
	// most one history window's worth rather than all 50 blocks.
	walPath := filepath.Join(cfg.DataDir, walName)
	if raw, err := os.ReadFile(walPath); err == nil {
		lines := 0
		for _, c := range string(raw) {
			if c == '\n' {
				lines++
			}
		}
		if lines == 0 || lines > 2*int(cfg.HistoryBlocks) {
			t.Fatalf("WAL should hold at most %d blocks above the image, got %d lines",
				2*cfg.HistoryBlocks, lines)
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, checkpointFile)); err != nil {
		t.Fatalf("checkpoint file missing: %v", err)
	}

	e2, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()

	ref, err := RebuildFromScratch(blocks, cfg.Metrics)
	if err != nil {
		t.Fatal(err)
	}
	// Recovery restores the whole retained window: the checkpoint image plus
	// the WAL replay up to the tip.
	for h := uint64(45); h <= 50; h++ {
		got, ok := e2.QueryAt(h)
		if !ok {
			t.Fatalf("missing recovered snapshot %d", h)
		}
		if !snapshotsEqual(got, ref[h]) {
			t.Fatalf("checkpoint recovery mismatch at %d", h)
		}
	}

	// Continue ingesting after recovery and verify continuity.
	more := makeBlocks(60)[50:]
	for _, b := range more {
		mustPush(t, e2, b)
	}
	all := makeBlocks(60)
	ref2, _ := RebuildFromScratch(all, cfg.Metrics)
	if !snapshotsEqual(mustSnap(t, e2, 60), ref2[60]) {
		t.Fatal("state diverges after checkpoint recovery + extension")
	}
}
