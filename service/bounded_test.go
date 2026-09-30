package service

import (
	"errors"
	"testing"

	"github.com/guoyyit/chain-metrics-go/domain"
)

// TestHistoryPruning drives the chain past the retention window and
// verifies recent snapshots stay available while old heights are
// reported as pruned, both from the in-memory ring and disk archive.
func TestHistoryPruning(t *testing.T) {
	cfg := testConfig(t)
	cfg.HistoryRetainBlocks = 8
	svc, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	cb := newChainBuilder()
	svc.IngestBlock(cb.next(1, mint("TOK", "a", "1000", "g", 0)))
	for h := uint64(2); h <= 40; h++ {
		if err := svc.IngestBlock(cb.next(h, transfer("TOK", "a", "b", "1", "t"+itoa(h), 0))); err != nil {
			t.Fatal(err)
		}
	}

	// Latest and the retained tail are queryable.
	if _, err := svc.QuerySnapshot(40); err != nil {
		t.Fatalf("latest: %v", err)
	}
	if _, err := svc.QuerySnapshot(39); err != nil {
		t.Fatalf("recent: %v", err)
	}

	// Very old height must be pruned (no unbounded retention).
	_, err = svc.QuerySnapshot(2)
	if !errors.Is(err, domain.ErrHeightPruned) {
		t.Fatalf("old height err = %v, want ErrHeightPruned", err)
	}
}

// TestZeroBalanceHoldersNotRetained sweeps an address down to zero and
// confirms it disappears from holders/tiers/window-independent ledger
// state, so long-lived zero addresses cannot grow memory.
func TestZeroBalanceHoldersNotRetained(t *testing.T) {
	cfg := testConfig(t)
	svc, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	cb := newChainBuilder()
	svc.IngestBlock(cb.next(1, mint("TOK", "ghost", "100", "m", 0)))
	svc.IngestBlock(cb.next(2, transfer("TOK", "ghost", "keeper", "100", "sweep", 0)))

	s, _ := svc.QuerySnapshot(2)
	tok := s.Tokens["TOK"]
	for _, h := range tok.TopHolders {
		if h.Address == "ghost" {
			t.Fatal("zero-balance address must not be retained as a holder")
		}
	}
	// Only keeper is tiered now.
	var holders uint64
	for _, c := range tok.BalanceTiers {
		holders += c
	}
	if holders != 1 {
		t.Fatalf("tiered holders = %d, want 1", holders)
	}
}

// TestQueryVisibilityRules checks out-of-range query semantics.
func TestQueryVisibilityRules(t *testing.T) {
	cfg := testConfig(t)
	svc, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	// No block published yet.
	if _, err := svc.QuerySnapshot(0); !errors.Is(err, domain.ErrHeightNotYetPublished) {
		t.Fatalf("empty query err = %v", err)
	}

	cb := newChainBuilder()
	svc.IngestBlock(cb.next(1, mint("TOK", "a", "10", "m", 0)))
	if _, err := svc.QuerySnapshot(2); !errors.Is(err, domain.ErrHeightNotYetPublished) {
		t.Fatalf("future query err = %v", err)
	}
	if _, err := svc.QueryToken(1, "MISSING"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing token err = %v", err)
	}
}
