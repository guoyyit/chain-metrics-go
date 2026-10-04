package chainmetrics

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestSmallHistoryBoundaries sweeps small HistoryBlocks values and both
// sides of the rollback boundary: a fork exactly at head-R must succeed, a
// fork one block below must be rejected, and behavior survives restart.
func TestSmallHistoryBoundaries(t *testing.T) {
	for _, r := range []uint64{1, 2, 3, 5} {
		t.Run("R="+uintToStr(r), func(t *testing.T) {
			dir := t.TempDir()
			cfg := testConfig()
			cfg.DataDir = filepath.Join(dir, "data")
			cfg.HistoryBlocks = r

			n := uint64(30)
			canonical := deepChain(n, "h")
			e, err := NewEngine(cfg)
			if err != nil {
				t.Fatal(err)
			}
			for _, b := range canonical {
				mustPush(t, e, b)
			}

			// Fork exactly at the rollback limit head-r: accepted.
			limit := n - r
			acceptedBase := canonical[limit-1]
			acceptedTail := forkChain(acceptedBase, limit, r+1, "ok")
			for _, b := range acceptedTail {
				if err := e.PushBlock(b); err != nil {
					t.Fatalf("boundary fork at %d rejected: %v", limit, err)
				}
			}
			wantHead := limit + r + 1
			if s, ok := e.QueryLatest(); !ok || s.BlockNumber != wantHead {
				t.Fatalf("after boundary fork head=%d want %d", s.BlockNumber, wantHead)
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}

			// Restart at the new branch tip must succeed and be consistent.
			e2, err := NewEngine(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer e2.Close()
			newChain := append(append([]Block{}, canonical[:limit]...), acceptedTail...)
			ref, _ := RebuildFromScratch(newChain, cfg.Metrics)
			for h := limit; h <= wantHead; h++ {
				got, ok := e2.QueryAt(h)
				if !ok || !snapshotsEqual(got, ref[h]) {
					t.Fatalf("post-restart boundary mismatch at %d", h)
				}
			}

			// A second fork one block below the new limit must be rejected.
			newLimit := wantHead - r
			if newLimit > 1 {
				// Build an orphan chain whose missing base sits one block
				// below the rollback limit. Its later blocks have heights
				// inside/above the window but the effective fork is too deep.
				baseHeight := newLimit - 1
				deep := block(baseHeight, "deep"+uintToStr(baseHeight),
					"missingAncestor"+uintToStr(baseHeight-1),
					ev("TKN", Transfer, "genesis", "x", 1, newLimit, 1))
				tail := forkChain(deep, baseHeight, r+2, "no")
				rejected := false
				for _, b := range tail {
					if err := e2.PushBlock(b); errors.Is(err, ErrReorgTooDeep) {
						rejected = true
					} else if err != nil {
						t.Fatalf("unexpected error %v", err)
					}
				}
				if !rejected {
					t.Fatalf("sub-limit fork at %d was not rejected", newLimit-1)
				}
				if s, ok := e2.QueryLatest(); !ok || s.BlockNumber != wantHead {
					t.Fatalf("rejected fork moved the head to %d", s.BlockNumber)
				}
			}
		})
	}
}

// TestIncrementalVsRebuildPerBlockWithFork runs a long chain that undergoes
// several reorganizations with out-of-order delivery, then asserts every
// queryable snapshot equals a genesis replay of the final canonical chain at
// the matching block hash, block by block.
func TestIncrementalVsRebuildPerBlockWithFork(t *testing.T) {
	cfg := testConfig()
	cfg.HistoryBlocks = 200
	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	canonical := deepChain(60, "h")
	for _, b := range canonical {
		mustPush(t, e, b)
	}
	// Fork 1 at height 20.
	f1 := forkChain(canonical[19], 20, 42, "a")
	mustPush(t, e, f1[len(f1)-1]) // tail first
	for _, b := range f1 {
		mustPush(t, e, b)
	}
	chain1 := append(append([]Block{}, canonical[:20]...), f1...)
	// Fork 2 at height 30 of the new branch, longer again.
	base2 := chain1[29]
	f2 := forkChain(base2, 30, 33, "b")
	for i := len(f2) - 1; i >= 0; i-- {
		mustPush(t, e, f2[i])
	}
	finalChain := append(append([]Block{}, chain1[:30]...), f2...)
	ref, _ := RebuildFromScratch(finalChain, cfg.Metrics)

	mismatch, ok, err := e.VerifyAgainstRebuild(finalChain)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("incremental vs rebuild mismatch at %d", mismatch)
	}
	// Explicit per-block, per-value comparison at every surviving height.
	for h := uint64(1); h <= 62; h++ {
		got, exists := e.QueryAt(h)
		if !exists {
			continue
		}
		if !snapshotsEqual(got, ref[h]) {
			t.Fatalf("block %d snapshot differs from genesis replay", h)
		}
	}
}
