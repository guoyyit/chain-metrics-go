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

	// WAL should retain only blocks after the latest checkpoint, i.e. fewer
	// than one history window's worth rather than all 50 blocks.
	walPath := filepath.Join(cfg.DataDir, walName)
	if raw, err := os.ReadFile(walPath); err == nil {
		lines := 0
		for _, c := range string(raw) {
			if c == '\n' {
				lines++
			}
		}
		if lines != 0 {
			t.Fatalf("WAL should be empty after a tip checkpoint, got %d lines", lines)
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
	// Recovery resumes at the last published (checkpointed) tip.
	got, ok := e2.QueryAt(50)
	if !ok {
		t.Fatal("missing recovered tip snapshot 50")
	}
	if !snapshotsEqual(got, ref[50]) {
		t.Fatal("checkpoint recovery mismatch at tip 50")
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
