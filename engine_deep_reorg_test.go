package chainmetrics

import (
	"errors"
	"path/filepath"
	"testing"
)

// deepReorgSetup builds a linear chain of length n. Each block mints one unit
// to a fresh address on top of an initial 1000 mint at block 1, making every
// height's metrics distinct.
func deepChain(n uint64, prefix string) []Block {
	const token = "TKN"
	name := func(i uint64) string { return prefix + uintToStr(i) }
	blocks := make([]Block, 0, n)
	for i := uint64(1); i <= n; i++ {
		var parent string
		if i > 1 {
			parent = name(i - 1)
		}
		events := []Event{}
		if i == 1 {
			events = append(events, ev(token, Mint, "", "genesis", 1000, 1, 0))
		}
		events = append(events, ev(token, Transfer, "genesis", name(i), 1, i, 1))
		blocks = append(blocks, block(i, name(i), parent, events...))
	}
	return blocks
}

// forkFrom returns an alternative branch that forks from base (the base block
// itself is shared) and extends for `length` blocks, each sending one unit to
// a distinct fork address.
func forkChain(base Block, baseDepth, length uint64, prefix string) []Block {
	const token = "TKN"
	out := make([]Block, 0, length)
	parent := base.Hash
	for i := uint64(1); i <= length; i++ {
		h := base.Number + i
		name := prefix + uintToStr(h)
		b := block(h, name, parent,
			ev(token, Transfer, "genesis", name, 2, h, 1))
		out = append(out, b)
		parent = name
	}
	_ = baseDepth
	return out
}

// TestDeepReorgAcrossCheckpoint: the fork point is older than the latest full
// state image (the WAL's oldest block) but still inside the retained rollback
// window. The reorganization must succeed and, after restart, every retained
// historical snapshot must equal a fresh genesis replay of the new chain.
func TestDeepReorgAcrossCheckpoint(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.HistoryBlocks = 10

	canonical := deepChain(30, "h")
	// With head at 30 and HistoryBlocks=10 the rollback limit is
	// 30-10+1 = 21 and the state image boundary is also 21, so a fork at
	// height 21 forks exactly at the oldest full image and must be accepted.
	forkBase := canonical[20]
	forkTail := forkChain(forkBase, 21, 10, "f")
	newChain := append(append([]Block{}, canonical[:21]...), forkTail...)
	ref, err := RebuildFromScratch(newChain, cfg.Metrics)
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
		for _, b := range forkTail {
			mustPush(t, e, b)
		}
		snap, ok := e.QueryLatest()
		if !ok || snap.BlockNumber != 31 || snap.BlockHash != "f31" {
			t.Fatalf("head after reorg = %d/%s, want 31/f31", snap.BlockNumber, snap.BlockHash)
		}
	}()

	// Restart and compare every retained historical snapshot, block by block,
	// against the genesis replay of the new chain.
	e2, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	for h := uint64(21); h <= 31; h++ {
		got, ok := e2.QueryAt(h)
		if !ok {
			t.Fatalf("missing snapshot at %d after restart", h)
		}
		if !snapshotsEqual(got, ref[h]) {
			t.Fatalf("post-restart snapshot mismatch at %d:\n got=%+v\nwant=%+v", h, got, ref[h])
		}
	}
	// The height-30 slot now belongs to the new branch; no old-branch content
	// may survive.
	if s, ok := e2.QueryAt(30); !ok || s.BlockHash != "f30" {
		t.Fatalf("height 30 must be the new-branch f30, got %+v", s)
	}
}

// TestReorgBeyondRetentionRejected: when the fork point is below the rollback
// limit the reorg is rejected with a distinguishable error; no published
// snapshot or durable data changes, including after restart, and ingestion of
// valid blocks keeps working.
func TestReorgBeyondRetentionRejected(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.HistoryBlocks = 5

	canonical := deepChain(20, "h")

	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for _, b := range canonical {
		mustPush(t, e, b)
	}
	// rollback limit = 20-5+1 = 16. Fork at height 10 must be rejected.
	forkBase := canonical[9]
	forkTail := forkChain(forkBase, 10, 20, "g")

	for _, b := range forkTail {
		err := e.PushBlock(b)
		if err == nil {
			t.Fatalf("deep fork block %s accepted silently", b.Hash)
		}
		if !errors.Is(err, ErrReorgTooDeep) {
			t.Fatalf("want ErrReorgTooDeep, got %v", err)
		}
	}

	// Head and every snapshot stay on the old branch.
	snap, ok := e.QueryLatest()
	if !ok || snap.BlockHash != "h20" {
		t.Fatalf("head changed after rejected reorg: %+v", snap)
	}
	ref, _ := RebuildFromScratch(canonical, cfg.Metrics)
	for h := uint64(16); h <= 20; h++ {
		got, ok := e.QueryAt(h)
		if !ok || !snapshotsEqual(got, ref[h]) {
			t.Fatalf("snapshot %d changed after rejected reorg", h)
		}
	}

	// Restart: still the old branch, and the rejected branch was never
	// durably written.
	e.Close()
	e2, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	got, ok := e2.QueryLatest()
	if !ok || got.BlockHash != "h20" {
		t.Fatalf("durable state contains rejected branch: %+v", got)
	}

	// Ingestion and queries continue normally.
	next := block(21, "h21", "h20",
		ev("TKN", Transfer, "genesis", "cont", 3, 21, 1))
	mustPush(t, e2, next)
	if s, ok := e2.QueryLatest(); !ok || s.BlockHash != "h21" {
		t.Fatalf("ingestion blocked after rejected reorg: %+v", s)
	}
}

// TestForkWithNoCommonAncestor verifies a fully disconnected branch (its
// ancestry never reaches any retained canonical block) is rejected with
// ErrNoCommonAncestor, distinctly from ErrReorgTooDeep.
func TestForkWithNoCommonAncestor(t *testing.T) {
	cfg := testConfig()
	cfg.HistoryBlocks = 10
	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for _, b := range deepChain(20, "h") {
		mustPush(t, e, b)
	}
	// A node already connected to the tree but whose ancestry shares nothing
	// with the canonical chain cannot be constructed through normal ingest
	// (parent unknown => orphan); simulate the connected-but-disjoint case by
	// building a separate root and asserting the engine surfaces the error via
	// the locator used by considerNode.
	other := &blockNode{
		number:      21,
		hash:        "x21",
		parentHash:  "x20",
		tokenStates: map[string]*tokenState{},
		windows:     map[string]*rollingWindow{},
	}
	e.chain.add(other)
	if _, err := e.locateFork(other); !errors.Is(err, ErrNoCommonAncestor) {
		t.Fatalf("want ErrNoCommonAncestor, got %v", err)
	}
}
