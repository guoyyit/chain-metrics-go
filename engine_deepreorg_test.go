package chainmetrics

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestReorgWithCheckpointImagePresent covers a reorganization whose rollback
// target stays inside the retained window while a complete state image has
// already been written during the run. The reorg must succeed and the
// recovered state after a restart must match a fresh replay of the new
// branch height by height.
func TestReorgWithCheckpointImagePresent(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.HistoryBlocks = 5

	canonical := genChain("h", 1, 12, "", 2)
	fork := genChain("g", 8, 13, "h7", 9)
	full := joinChains(canonical[:7], fork)
	ref, err := RebuildFromScratch(full, cfg.Metrics)
	if err != nil {
		t.Fatal(err)
	}

	func() {
		e, err := NewEngine(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer e.Close()
		for _, b := range canonical {
			mustPush(t, e, b)
		}
		// A complete state image must exist before the fork arrives.
		if e.checkpointHeight == 0 {
			t.Fatal("expected a checkpoint image before the reorg")
		}
		if e.checkpointHeight >= 7 {
			t.Fatalf("test setup: image at %d should be below fork point 7", e.checkpointHeight)
		}
		for _, b := range fork {
			mustPush(t, e, b)
		}
		// The reorg must have switched the head to the fork branch.
		latest, ok := e.QueryLatest()
		if !ok || latest.BlockHash != "g13" {
			t.Fatalf("head after reorg = %s, want g13", latest.BlockHash)
		}
		assertWindowMatchesRef(t, e, ref, 8, 13)
	}()

	// After a restart every snapshot in the retained window must be intact.
	e2, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("restart after reorg: %v", err)
	}
	defer e2.Close()
	assertWindowMatchesRef(t, e2, ref, 8, 13)

	// Ingestion continues on the recovered branch.
	more := genChain("g", 14, 16, "g13", 9)
	for _, b := range more {
		mustPush(t, e2, b)
	}
	ref2, err := RebuildFromScratch(joinChains(full, more), cfg.Metrics)
	if err != nil {
		t.Fatal(err)
	}
	assertWindowMatchesRef(t, e2, ref2, 11, 16)

	// The incremental state must equal a full replay of the merged chain.
	if mismatch, ok, err := e2.VerifyAgainstRebuild(joinChains(full, more)); err != nil || !ok {
		t.Fatalf("VerifyAgainstRebuild: mismatch=%d ok=%v err=%v", mismatch, ok, err)
	}
}

// TestReorgBeyondRetentionRejected covers forks whose rollback target is no
// longer retained: the push must fail with a distinguishable error and must
// not alter any published snapshot, durable state, or later ingestion.
func TestReorgBeyondRetentionRejected(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.HistoryBlocks = 5

	canonical := genChain("h", 1, 12, "", 2)
	ref, err := RebuildFromScratch(canonical, cfg.Metrics)
	if err != nil {
		t.Fatal(err)
	}

	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range canonical {
		mustPush(t, e, b)
	}
	// floor = 12 - 5 = 7: a fork at h6 is below the retained window.
	err = e.PushBlock(block(7, "f7", "h6", ev("TKN", Mint, "", "Z", 1, 7, 0)))
	if !errors.Is(err, ErrReorgTooDeep) {
		t.Fatalf("err = %v, want ErrReorgTooDeep", err)
	}
	// A whole overtaking branch on the same pruned ancestor is rejected too.
	err = e.PushBlock(block(8, "f8", "f7", ev("TKN", Mint, "", "Z", 1, 8, 0)))
	if !errors.Is(err, ErrReorgTooDeep) {
		t.Fatalf("err = %v, want ErrReorgTooDeep", err)
	}
	// State is completely untouched.
	assertWindowMatchesRef(t, e, ref, 7, 12)
	if latest, _ := e.QueryLatest(); latest.BlockHash != "h12" {
		t.Fatalf("head moved to %s after rejected reorg", latest.BlockHash)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart recovers the exact pre-rejection chain.
	e2, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("restart after rejection: %v", err)
	}
	defer e2.Close()
	assertWindowMatchesRef(t, e2, ref, 7, 12)

	// Ingestion and queries are unaffected afterwards.
	more := genChain("h", 13, 14, "h12", 2)
	for _, b := range more {
		mustPush(t, e2, b)
	}
	ref2, err := RebuildFromScratch(joinChains(canonical, more), cfg.Metrics)
	if err != nil {
		t.Fatal(err)
	}
	assertWindowMatchesRef(t, e2, ref2, 9, 14)
}

// TestReorgBeyondRetentionRejectedAfterWindowSlides builds a fork branch
// while it is still retained, lets the retention window slide past the fork
// point, and then extends the branch past the head. The overtaking push must
// be rejected with ErrReorgTooDeep rather than silently ignored.
func TestReorgBeyondRetentionRejectedAfterWindowSlides(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.HistoryBlocks = 5

	canonical := genChain("h", 1, 10, "", 2)
	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range canonical {
		mustPush(t, e, b)
	}
	// Fork at h5 (floor is 10-5=5): branch blocks g6..g9 are retained.
	branch := genChain("g", 6, 9, "h5", 7)
	for _, b := range branch {
		mustPush(t, e, b)
	}
	// The canonical chain advances; the window slides past the fork point.
	more := genChain("h", 11, 14, "h10", 2)
	for _, b := range more {
		mustPush(t, e, b)
	}
	// Extending the branch past the head must fail explicitly.
	overtake := genChain("g", 10, 15, "g9", 7)
	var lastErr error
	for _, b := range overtake {
		lastErr = e.PushBlock(b)
	}
	if !errors.Is(lastErr, ErrReorgTooDeep) {
		t.Fatalf("err = %v, want ErrReorgTooDeep", lastErr)
	}

	ref, err := RebuildFromScratch(joinChains(canonical, more), cfg.Metrics)
	if err != nil {
		t.Fatal(err)
	}
	assertWindowMatchesRef(t, e, ref, 9, 14)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e2, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("restart after rejection: %v", err)
	}
	defer e2.Close()
	assertWindowMatchesRef(t, e2, ref, 9, 14)
}

// TestReorgThenRestartAllSnapshots captures every queryable snapshot right
// after a reorg, restarts, and requires every height to be identical.
func TestReorgThenRestartAllSnapshots(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.HistoryBlocks = 4

	canonical := genChain("h", 1, 10, "", 3)
	// Fork at h6: depth 10-6 = 4 equals HistoryBlocks exactly.
	fork := genChain("g", 7, 12, "h6", 8)
	full := joinChains(canonical[:6], fork)
	ref, err := RebuildFromScratch(full, cfg.Metrics)
	if err != nil {
		t.Fatal(err)
	}

	before := make(map[uint64]Snapshot)
	func() {
		e, err := NewEngine(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer e.Close()
		for _, b := range canonical {
			mustPush(t, e, b)
		}
		for _, b := range fork {
			mustPush(t, e, b)
		}
		for h := uint64(8); h <= 12; h++ {
			snap, ok := e.QueryAt(h)
			if !ok {
				t.Fatalf("missing snapshot %d before restart", h)
			}
			before[h] = snap
		}
	}()

	e2, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer e2.Close()
	for h := uint64(8); h <= 12; h++ {
		got, ok := e2.QueryAt(h)
		if !ok {
			t.Fatalf("snapshot %d lost after restart", h)
		}
		if !snapshotsEqual(got, before[h]) {
			t.Fatalf("snapshot %d changed across restart", h)
		}
		if !snapshotsEqual(got, ref[h]) {
			t.Fatalf("snapshot %d diverges from fresh replay", h)
		}
	}
}
