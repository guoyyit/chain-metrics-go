package chainmetrics

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestSmallHistoryBoundary exercises reorgs with tiny retention windows,
// including a rollback depth exactly equal to HistoryBlocks (must succeed)
// and one block deeper (must be rejected), with restarts in between.
func TestSmallHistoryBoundary(t *testing.T) {
	for _, history := range []uint64{1, 2, 3} {
		history := history
		t.Run(uintToStr(history), func(t *testing.T) {
			dir := t.TempDir()
			cfg := testConfig()
			cfg.DataDir = filepath.Join(dir, "data")
			cfg.HistoryBlocks = history

			n := 2*history + 2
			forkAt := n - history // depth exactly == HistoryBlocks
			canonical := genChain("h", 1, n, "", 2)
			fork := genChain("g", forkAt+1, n+1, "h"+uintToStr(forkAt), 6)
			full := joinChains(canonical[:forkAt], fork)
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
				for _, b := range fork {
					mustPush(t, e, b)
				}
				latest, ok := e.QueryLatest()
				if !ok || latest.BlockHash != "g"+uintToStr(n+1) {
					t.Fatalf("head = %s, want g%d", latest.BlockHash, n+1)
				}
				assertWindowMatchesRef(t, e, ref, forkAt+1, n+1)

				// One block deeper than the window: explicit rejection.
				err = e.PushBlock(block(forkAt, "f"+uintToStr(forkAt),
					"h"+uintToStr(forkAt-1), ev("TKN", Mint, "", "Z", 1, forkAt, 0)))
				if !errors.Is(err, ErrReorgTooDeep) {
					t.Fatalf("err = %v, want ErrReorgTooDeep", err)
				}
				assertWindowMatchesRef(t, e, ref, forkAt+1, n+1)
			}()

			e2, err := NewEngine(cfg)
			if err != nil {
				t.Fatalf("restart: %v", err)
			}
			defer e2.Close()
			assertWindowMatchesRef(t, e2, ref, forkAt+1, n+1)
		})
	}
}

// TestReorgReplayCrossesCheckpoints reorganizes to a branch long enough that
// checkpoint images are written while the reorg replay runs, then crashes at
// every durable stage and requires consistent recovery.
func TestReorgReplayCrossesCheckpoints(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.HistoryBlocks = 3

	canonical := genChain("h", 1, 6, "", 2)
	fork := genChain("g", 4, 16, "h3", 6)
	oldRef, err := RebuildFromScratch(canonical, cfg.Metrics)
	if err != nil {
		t.Fatal(err)
	}
	newRef, err := RebuildFromScratch(joinChains(canonical[:3], fork), cfg.Metrics)
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
			snaps[stage+uintToStr(uint64(counts[stage]))] = copyDirFiles(t, cfg.DataDir)
		}
		for _, b := range canonical {
			mustPush(t, e, b)
		}
		for _, b := range fork {
			mustPush(t, e, b)
		}
		// The replay must have written at least one image on the new branch.
		if e.checkpointHeight == 0 {
			t.Fatal("expected checkpoints during the long reorg replay")
		}
		assertWindowMatchesRef(t, e, newRef, 13, 16)
	}()

	if counts["checkpoint.image"] == 0 {
		t.Fatal("no checkpoint stage captured during reorg replay")
	}
	for key, snapDir := range snaps {
		t.Run(key, func(t *testing.T) {
			assertRecoveredConsistent(t, snapDir, cfg, oldRef, newRef)
		})
	}

	// A final restart lands on the new branch with the full window intact.
	e2, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer e2.Close()
	assertWindowMatchesRef(t, e2, newRef, 13, 16)
}
