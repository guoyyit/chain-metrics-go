package chainmetrics

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copyDirFiles copies every regular file in src into a fresh temp dir and
// returns its path. The copy is a consistent crash image because every
// durable write is flushed/synced before a hook stage fires.
func copyDirFiles(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, entry.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

// assertRecoveredConsistent opens an engine on dir and requires the
// recovered chain to be complete and consistent: every snapshot in the
// retained window must match the reference of the chain the head belongs to.
func assertRecoveredConsistent(t *testing.T, dir string, cfg Config, oldRef, newRef map[uint64]Snapshot) {
	t.Helper()
	c := cfg
	c.DataDir = dir
	e, err := NewEngine(c)
	if err != nil {
		t.Fatalf("recovery from %s failed: %v", filepath.Base(dir), err)
	}
	defer e.Close()
	latest, ok := e.QueryLatest()
	if !ok {
		return // nothing was ever committed; an empty engine is consistent
	}
	ref := oldRef
	if strings.HasPrefix(latest.BlockHash, "g") {
		ref = newRef
	}
	head := latest.BlockNumber
	low := uint64(1)
	if head > cfg.HistoryBlocks {
		low = head - cfg.HistoryBlocks
	}
	assertWindowMatchesRef(t, e, ref, low, head)
}

// TestCrashAtEveryDurableStage snapshots the data directory at every
// durable-state transition of a run that includes checkpoints and a deep
// reorganization, then requires recovery from every single snapshot to
// succeed and to match an uninterrupted replay of one of the two chains.
func TestCrashAtEveryDurableStage(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.HistoryBlocks = 5

	canonical := genChain("h", 1, 12, "", 2)
	fork := genChain("g", 8, 13, "h7", 9)
	oldRef, err := RebuildFromScratch(canonical, cfg.Metrics)
	if err != nil {
		t.Fatal(err)
	}
	newRef, err := RebuildFromScratch(joinChains(canonical[:7], fork), cfg.Metrics)
	if err != nil {
		t.Fatal(err)
	}

	snaps := make(map[string]string)
	counts := make(map[string]int)
	func() {
		e, err := NewEngine(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer e.Close()
		e.hook = func(stage string) {
			counts[stage]++
			key := fmt.Sprintf("%s#%d", stage, counts[stage])
			snaps[key] = copyDirFiles(t, cfg.DataDir)
		}
		for _, b := range canonical {
			mustPush(t, e, b)
		}
		for _, b := range fork {
			mustPush(t, e, b)
		}
	}()

	// The run must have produced checkpoint and reorg stages to be meaningful.
	if counts["checkpoint.image"] == 0 {
		t.Fatal("no checkpoint stage captured")
	}
	if counts["reorg.branch"] == 0 || counts["reorg.head"] == 0 {
		t.Fatal("no reorg stage captured")
	}

	for key, snapDir := range snaps {
		t.Run(key, func(t *testing.T) {
			assertRecoveredConsistent(t, snapDir, cfg, oldRef, newRef)
		})
	}
}

// TestCrashDuringReorgStages verifies the two-sided commit semantics of a
// reorganization: a crash before the head pointer moves recovers the old
// chain; a crash after it recovers the new chain. Both must allow ingestion
// to continue.
func TestCrashDuringReorgStages(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.HistoryBlocks = 5

	canonical := genChain("h", 1, 12, "", 2)
	fork := genChain("g", 8, 13, "h7", 9)
	oldRef, err := RebuildFromScratch(joinChains(canonical, genChain("h", 13, 13, "h12", 2)), cfg.Metrics)
	if err != nil {
		t.Fatal(err)
	}
	newFull := joinChains(canonical[:7], fork)
	newRef, err := RebuildFromScratch(joinChains(newFull, genChain("g", 14, 14, "g13", 9)), cfg.Metrics)
	if err != nil {
		t.Fatal(err)
	}

	snaps := make(map[string]string)
	func() {
		e, err := NewEngine(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer e.Close()
		e.hook = func(stage string) {
			if strings.HasPrefix(stage, "reorg.") {
				snaps[stage] = copyDirFiles(t, cfg.DataDir)
			}
		}
		for _, b := range canonical {
			mustPush(t, e, b)
		}
		for _, b := range fork {
			mustPush(t, e, b)
		}
	}()

	if len(snaps) == 0 {
		t.Fatal("no reorg stages captured")
	}
	for stage, snapDir := range snaps {
		t.Run(stage, func(t *testing.T) {
			c := cfg
			c.DataDir = snapDir
			e, err := NewEngine(c)
			if err != nil {
				t.Fatalf("recovery failed: %v", err)
			}
			defer e.Close()
			latest, ok := e.QueryLatest()
			if !ok {
				t.Fatal("recovered engine has no head")
			}
			switch latest.BlockHash {
			case "h12":
				// Pre-commit: the old chain is fully intact and extends.
				mustPush(t, e, block(13, "h13", "h12",
					ev("TKN", Mint, "", "A", 2, 13, 0),
					ev("TKN", Transfer, "A", "B", 1, 13, 1),
					ev("TKN", Transfer, "B", "A", 1, 13, 2)))
				assertWindowMatchesRef(t, e, oldRef, 8, 13)
			case "g13":
				// Post-commit: the new chain is fully intact and extends.
				mustPush(t, e, block(14, "g14", "g13",
					ev("TKN", Mint, "", "A", 9, 14, 0),
					ev("TKN", Transfer, "A", "B", 1, 14, 1),
					ev("TKN", Transfer, "B", "A", 1, 14, 2)))
				assertWindowMatchesRef(t, e, newRef, 9, 14)
			default:
				t.Fatalf("recovered head %s is neither old nor new tip", latest.BlockHash)
			}
		})
	}
}
