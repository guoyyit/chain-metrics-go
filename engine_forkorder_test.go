package chainmetrics

import (
	"errors"
	"math/rand"
	"path/filepath"
	"testing"
)

// shuffled returns a deterministically permuted copy of blocks.
func shuffled(blocks []Block, seed int64) []Block {
	out := append([]Block{}, blocks...)
	r := rand.New(rand.NewSource(seed))
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// reversed returns blocks in reverse order.
func reversed(blocks []Block) []Block {
	out := make([]Block, len(blocks))
	for i, b := range blocks {
		out[len(blocks)-1-i] = b
	}
	return out
}

// TestOutOfOrderForkArrival delivers a fork branch in every order and
// requires the final reorganization result to be identical to in-order
// delivery and to a fresh replay of the branch.
func TestOutOfOrderForkArrival(t *testing.T) {
	cfg := testConfig()

	canonical := genChain("h", 1, 10, "", 2)
	fork := genChain("g", 4, 12, "h3", 6)
	full := joinChains(canonical[:3], fork)
	ref, err := RebuildFromScratch(full, cfg.Metrics)
	if err != nil {
		t.Fatal(err)
	}

	// In-order delivery is the baseline every other order must match.
	baseline := make(map[uint64]Snapshot)
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
		for h := uint64(1); h <= 12; h++ {
			snap, ok := e.QueryAt(h)
			if !ok {
				t.Fatalf("baseline missing snapshot %d", h)
			}
			baseline[h] = snap
		}
	}()

	middleLate := []Block{fork[3], fork[4], fork[0], fork[8], fork[7], fork[1], fork[6], fork[2], fork[5]}
	orders := map[string][]Block{
		"reversed":    reversed(fork),
		"middle-late": middleLate,
		"shuffle-1":   shuffled(fork, 1),
		"shuffle-2":   shuffled(fork, 42),
		"shuffle-3":   shuffled(fork, 2024),
	}
	for name, order := range orders {
		t.Run(name, func(t *testing.T) {
			e, err := NewEngine(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			for _, b := range canonical {
				mustPush(t, e, b)
			}
			for _, b := range order {
				mustPush(t, e, b)
				mustPush(t, e, b) // duplicate delivery must be idempotent
			}
			latest, ok := e.QueryLatest()
			if !ok || latest.BlockHash != "g12" {
				t.Fatalf("head = %s, want g12", latest.BlockHash)
			}
			for h := uint64(1); h <= 12; h++ {
				got, ok := e.QueryAt(h)
				if !ok {
					t.Fatalf("missing snapshot %d", h)
				}
				if !snapshotsEqual(got, baseline[h]) {
					t.Fatalf("height %d differs from in-order delivery", h)
				}
				if !snapshotsEqual(got, ref[h]) {
					t.Fatalf("height %d differs from fresh replay", h)
				}
			}
		})
	}
}

// TestOutOfOrderForkArrivalWithRestart shuffles a fork branch while loose
// events and duplicates mix in, then verifies consistency across a restart.
func TestOutOfOrderForkArrivalWithRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.HistoryBlocks = 6

	canonical := genChain("h", 1, 10, "", 2)
	fork := genChain("g", 6, 13, "h5", 6)
	full := joinChains(canonical[:5], fork)
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
		// Loose events of two fork blocks arrive before any fork block.
		for _, ev := range fork[2].Events {
			if err := e.PushEvent(ev); err != nil {
				t.Fatal(err)
			}
		}
		for _, b := range shuffled(fork, 7) {
			mustPush(t, e, b)
		}
		// Re-deliver fork blocks still inside the retained window: they are
		// idempotent no-ops.
		for _, b := range fork[1:3] {
			mustPush(t, e, b)
		}
		// Re-delivering a block that has fallen below the retention window
		// is indistinguishable from a deep fork: it is rejected explicitly
		// and leaves every snapshot untouched.
		if err := e.PushBlock(fork[0]); !errors.Is(err, ErrReorgTooDeep) {
			t.Fatalf("re-delivery below window: err = %v, want ErrReorgTooDeep", err)
		}
		for _, ev := range fork[2].Events {
			if err := e.PushEvent(ev); err != nil {
				t.Fatal(err)
			}
		}
		assertWindowMatchesRef(t, e, ref, 7, 13)
	}()

	e2, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer e2.Close()
	assertWindowMatchesRef(t, e2, ref, 7, 13)
}
