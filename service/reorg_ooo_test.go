package service

import (
	"testing"

	"github.com/guoyyit/chain-metrics-go/domain"
)

// TestForkDeliveredTipFirst delivers a longer competing branch tip
// before any of its ancestors, then fills ancestors in. The service
// must buffer the disconnected tip and only reorg once the branch is
// complete; the final state must match the in-order result.
func TestForkDeliveredTipFirst(t *testing.T) {
	svc, err := Open(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	cb := newChainBuilder()
	a1 := cb.next(1, mint("TOK", "alice", "1000", "g", 0))
	a2 := cb.next(2, transfer("TOK", "alice", "bob", "10", "a2", 0))
	a3 := cb.next(3, transfer("TOK", "alice", "bob", "10", "a3", 0))
	for _, b := range []*domain.Block{a1, a2, a3} {
		if err := svc.IngestBlock(b); err != nil {
			t.Fatal(err)
		}
	}

	// Competing chain at heights 3,4,5 (tip 5 > head 3).
	x3 := cb.forkFrom(a2, 3, transfer("TOK", "alice", "x", "100", "x3", 0))
	x4 := cb.forkFrom(x3, 4, transfer("TOK", "x", "y", "40", "x4", 0))
	x5 := cb.forkFrom(x4, 5, burn("TOK", "alice", "5", "x5", 0))

	// Tip first, then middle, then base.
	for _, b := range []*domain.Block{x5, x4} {
		if err := svc.IngestBlock(b); err != nil {
			t.Fatalf("buffering %d: %v", b.Height, err)
		}
		if svc.Head().Height != 3 {
			t.Fatalf("head must not move on incomplete branch, got %d", svc.Head().Height)
		}
	}
	if err := svc.IngestBlock(x3); err != nil {
		t.Fatalf("fork base: %v", err)
	}
	if svc.Head().Hash != x5.Hash {
		t.Fatalf("head = %s, want %s", svc.Head().Hash, x5.Hash)
	}
	snap, err := svc.QuerySnapshot(5)
	if err != nil {
		t.Fatal(err)
	}
	tok := snap.Tokens["TOK"]
	// alice: 1000-10-100-5 = 885; bob 10; x 60; y 40.
	want := map[string]string{"alice": "885", "bob": "10", "x": "60", "y": "40"}
	got := map[string]string{}
	for _, h := range tok.TopHolders {
		got[h.Address] = h.Balance.String()
	}
	for addr, w := range want {
		if got[addr] != w {
			t.Fatalf("%s=%s want %s (all=%v)", addr, got[addr], w, got)
		}
	}

	// And it must equal a fresh in-order run independently.
	svc2, err := Open(func() domain.Config {
		c := testConfig(t)
		c.DataDir = svc.cfg.DataDir + "-ref"
		return c
	}())
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	for _, b := range []*domain.Block{a1, a2, x3, x4, x5} {
		if err := svc2.IngestBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	ref, err := svc2.QuerySnapshot(5)
	if err != nil {
		t.Fatal(err)
	}
	if snap.BlockHash != ref.BlockHash || snap.Tokens["TOK"].CirculatingSupply.Cmp(ref.Tokens["TOK"].CirculatingSupply) != 0 {
		t.Fatal("tip-first and in-order results differ")
	}
}
