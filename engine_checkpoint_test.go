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

	// The state image is published at the finalized boundary head -
	// HistoryBlocks (=45), not at the tip. The WAL retains the canonical
	// blocks strictly above the image up to the head, i.e. at most
	// HistoryBlocks blocks, while still allowing a rollback to every height
	// the engine advertises support for.
	walPath := filepath.Join(cfg.DataDir, walName)
	if raw, err := os.ReadFile(walPath); err == nil {
		lines := 0
		maxHeight := uint64(0)
		minHeight := uint64(0)
		first := true
		for _, c := range string(raw) {
			if c == '\n' {
				lines++
			}
		}
		store, err := OpenStore(cfg.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		pbs, err := store.walBlocks()
		store.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, pb := range pbs {
			if first || pb.Number < minHeight {
				minHeight = pb.Number
			}
			if pb.Number > maxHeight {
				maxHeight = pb.Number
			}
			first = false
		}
		if lines > int(cfg.HistoryBlocks)+1 {
			t.Fatalf("WAL grew beyond the bounded window: %d lines", lines)
		}
		if lines != 0 {
			if minHeight != uint64(50)-cfg.HistoryBlocks {
				t.Fatalf("WAL starts at %d, want %d", minHeight, 50-cfg.HistoryBlocks)
			}
			if maxHeight != 50 {
				t.Fatalf("WAL must retain through the tip, max height %d", maxHeight)
			}
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
	// Recovery resumes at the tip by replaying the bounded WAL on top of the
	// finalized state image.
	got, ok := e2.QueryAt(50)
	if !ok {
		t.Fatal("missing recovered tip snapshot 50")
	}
	if !snapshotsEqual(got, ref[50]) {
		t.Fatal("checkpoint recovery mismatch at tip 50")
	}
	// Every retained historical snapshot, compared block by block against a
	// genesis replay, must survive recovery.
	for h := uint64(50) - cfg.HistoryBlocks; h <= 50; h++ {
		got, ok := e2.QueryAt(h)
		if !ok || !snapshotsEqual(got, ref[h]) {
			t.Fatalf("recovered historical snapshot mismatch at %d", h)
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
