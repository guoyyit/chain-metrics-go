package chainmetrics

import "testing"

// TestForkBlocksArriveOutOfOrder delivers a winning fork tail-first: the
// high-height tip arrives before the intermediate fork blocks. The eventual
// reorganization must produce exactly the same per-block snapshots as an
// in-order delivery and as a genesis replay of the new chain.
func TestForkBlocksArriveOutOfOrder(t *testing.T) {
	cfg := testConfig()
	cfg.HistoryBlocks = 40
	canonical := deepChain(12, "h")
	forkBase := canonical[4] // fork point height 5
	forkTail := forkChain(forkBase, 5, 10, "f")
	newChain := append(append([]Block{}, canonical[:5]...), forkTail...)
	ref, err := RebuildFromScratch(newChain, cfg.Metrics)
	if err != nil {
		t.Fatal(err)
	}

	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	for _, b := range canonical {
		mustPush(t, e, b)
	}
	// Deliver the fork tail-first: tip first, then descending toward the base.
	for i := len(forkTail) - 1; i >= 0; i-- {
		mustPush(t, e, forkTail[i])
	}

	snap, ok := e.QueryLatest()
	if !ok || snap.BlockNumber != 15 || snap.BlockHash != "f15" {
		t.Fatalf("head = %d/%s, want 15/f15", snap.BlockNumber, snap.BlockHash)
	}
	for h := uint64(5); h <= 15; h++ {
		got, ok := e.QueryAt(h)
		if !ok {
			t.Fatalf("missing snapshot at %d", h)
		}
		if !snapshotsEqual(got, ref[h]) {
			t.Fatalf("out-of-order fork mismatch at %d", h)
		}
	}
}

// TestForkInterleavedArrival interleaves fork and canonical blocks and
// re-delivers blocks: every result must remain idempotent and equal to the
// ordered replay.
func TestForkInterleavedArrival(t *testing.T) {
	cfg := testConfig()
	cfg.HistoryBlocks = 40
	canonical := deepChain(10, "h")
	forkBase := canonical[3]
	forkTail := forkChain(forkBase, 4, 9, "f")
	newChain := append(append([]Block{}, canonical[:4]...), forkTail...)
	ref, _ := RebuildFromScratch(newChain, cfg.Metrics)

	e, _ := NewEngine(cfg)
	defer e.Close()

	mustPush(t, e, canonical[0])
	mustPush(t, e, forkTail[len(forkTail)-1]) // tip of the fork, orphan
	mustPush(t, e, canonical[1])
	mustPush(t, e, forkTail[3])
	mustPush(t, e, canonical[2])
	mustPush(t, e, canonical[3])
	mustPush(t, e, forkTail[0])
	mustPush(t, e, forkTail[1])
	mustPush(t, e, forkTail[2])
	for i := 4; i < len(canonical); i++ {
		mustPush(t, e, canonical[i])
	}
	for i := 4; i < len(forkTail); i++ {
		mustPush(t, e, forkTail[i])
	}
	// Re-deliver everything: must be a no-op.
	for _, b := range newChain {
		mustPush(t, e, b)
	}
	for h := uint64(4); h <= 13; h++ {
		got, ok := e.QueryAt(h)
		if !ok || !snapshotsEqual(got, ref[h]) {
			t.Fatalf("interleaved fork mismatch at %d", h)
		}
	}
}
