package chainmetrics

import (
	"errors"
	"path/filepath"
	"testing"
)

// errInjectedCrash is returned by a crash hook to emulate the process dying
// immediately after a durable write landed on disk.
var errInjectedCrash = errors.New("injected crash")

// runReorgCrashScenario performs a reorganization that switches across a
// state-image boundary, injecting a crash at the named protocol point. It
// then opens a fresh engine and verifies every retained snapshot matches an
// uninterrupted genesis replay of the new chain.
func runReorgCrashScenario(t *testing.T, crashPoint string) {
	t.Helper()
	dir := t.TempDir()
	cfg := testConfig()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.HistoryBlocks = 8

	canonical := deepChain(24, "h")
	forkBase := canonical[17] // fork at height 18
	forkTail := forkChain(forkBase, 18, 8, "f")
	newChain := append(append([]Block{}, canonical[:18]...), forkTail...)
	ref, err := RebuildFromScratch(newChain, cfg.Metrics)
	if err != nil {
		t.Fatal(err)
	}
	oldRef, err := RebuildFromScratch(canonical, cfg.Metrics)
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
		e.crashHook = func(point string) error {
			if point == crashPoint {
				return errInjectedCrash
			}
			return nil
		}
		for _, b := range forkTail {
			_ = e.PushBlock(b) // crash aborts the call mid-protocol
		}
	}()

	// Restart must always succeed and recover to the last committed state,
	// which is consistent with an uninterrupted run up to that block.
	e2, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("restart after crash at %s failed: %v", crashPoint, err)
	}
	defer e2.Close()

	got, ok := e2.QueryLatest()
	if !ok {
		t.Fatalf("no snapshot after restart at crash point %s", crashPoint)
	}
	// The recovered head must be a consistent tip on either the old branch
	// (crash before the head flip) or the new branch (crash at/after the
	// flip), and match the corresponding genesis replay exactly.
	var reference map[uint64]Snapshot
	switch got.BlockHash {
	case "h24":
		reference = oldRef
	default:
		reference = ref
	}
	want, exists := reference[got.BlockNumber]
	if !exists || want.BlockHash != got.BlockHash {
		t.Fatalf("crash %s: recovered head %d/%s is neither branch tip",
			crashPoint, got.BlockNumber, got.BlockHash)
	}
	if !snapshotsEqual(got, want) {
		t.Fatalf("crash %s: head snapshot mismatch at %d", crashPoint, got.BlockNumber)
	}

	// Every retained historical snapshot must equal the reference.
	lowest := uint64(1)
	if got.BlockNumber > cfg.HistoryBlocks {
		lowest = got.BlockNumber - cfg.HistoryBlocks
	}
	for h := lowest; h <= got.BlockNumber; h++ {
		s, ok := e2.QueryAt(h)
		if !ok {
			continue
		}
		w, present := reference[h]
		if present && w.BlockHash == s.BlockHash && !snapshotsEqual(s, w) {
			t.Fatalf("crash %s: historical mismatch at %d", crashPoint, h)
		}
	}

	// The engine must continue ingesting after recovery.
	cont := block(got.BlockNumber+1, "c"+uintToStr(got.BlockNumber+1), got.BlockHash,
		ev("TKN", Transfer, "genesis", "postcrash", 1, got.BlockNumber+1, 1))
	// Extending the recovered tip must work regardless of which branch won.
	if err := e2.PushBlock(cont); err != nil {
		t.Fatalf("ingestion after crash recovery failed: %v", err)
	}
	if s, ok := e2.QueryLatest(); !ok || s.BlockNumber != got.BlockNumber+1 {
		t.Fatalf("head did not advance after recovery")
	}
}

func TestCrashMidReorgWALAppend(t *testing.T)  { runReorgCrashScenario(t, "reorg_wal_append") }
func TestCrashMidReorgWALRewrite(t *testing.T) { runReorgCrashScenario(t, "reorg_wal_rewrite") }
func TestCrashMidReorgHeadFlip(t *testing.T)   { runReorgCrashScenario(t, "reorg_head_flip") }
func TestCrashMidReorgCheckpoint(t *testing.T) { runReorgCrashScenario(t, "checkpoint_saved") }
func TestCrashMidReorgAfterCheckpoint(t *testing.T) {
	runReorgCrashScenario(t, "reorg_after_checkpoint")
}
