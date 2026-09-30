package service

import (
	"testing"

	"github.com/guoyyit/chain-metrics-go/domain"
	"github.com/guoyyit/chain-metrics-go/store"
)

// TestReorgCrashRecovery ingests a complete competing branch up to the
// new tip, then simulates a crash at two points:
//
//  1. the reorg intent is on disk but neither archive nor head moved;
//  2. (covered by ordinary restart) the intent completed normally.
//
// Recovery must finish the switch to the new tip.
func TestReorgCrashRecovery(t *testing.T) {
	cfg := testConfig(t)
	svc, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}

	cb := newChainBuilder()
	b1 := cb.next(1, mint("TOK", "alice", "1000", "g", 0))
	b2 := cb.next(2, transfer("TOK", "alice", "bob", "10", "a2", 0))
	b3 := cb.next(3, transfer("TOK", "alice", "bob", "10", "a3", 0))
	for _, b := range []*domain.Block{b1, b2, b3} {
		if err := svc.IngestBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	x2 := cb.forkFrom(b1, 2, transfer("TOK", "alice", "x", "20", "x2", 0))
	x3 := cb.forkFrom(x2, 3, burn("TOK", "alice", "7", "x3", 0))
	for _, b := range []*domain.Block{x2, x3} {
		if err := svc.IngestBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	if svc.Head().Hash != x3.Hash {
		t.Fatalf("precondition head=%s want %s", svc.Head().Hash, x3.Hash)
	}
	svc.Close()

	// Reopen on a second engine instance over the same dir: recovery
	// must be a no-op-equivalent stable state at x3.
	svc, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if svc.Head().Hash != x3.Hash {
		t.Fatalf("head after restart = %s", svc.Head().Hash)
	}
	if err := svc.VerifyIncremental(); err != nil {
		t.Fatalf("verify: %v", err)
	}
	svc.Close()

	// Now simulate an interrupted reorg on a fresh branch y2/y3:
	// write both blocks, keep head at x3, and only persist the intent.
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	y2 := cb.forkFrom(b1, 2, transfer("TOK", "alice", "y", "40", "y2", 0))
	y3 := cb.forkFrom(y2, 3, transfer("TOK", "y", "z", "15", "y3", 0))
	for _, b := range []*domain.Block{y2, y3} {
		if err := st.SaveBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	// Head remains on x3; intent points at y3.
	if err := st.SaveHead(domain.HeadPointer{Height: 3, Hash: x3.Hash}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveReorgIntent(domain.HeadPointer{Height: 3, Hash: y3.Hash}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	svc, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if svc.Head().Hash != y3.Hash {
		t.Fatalf("after interrupted-reorg recovery head=%s want %s", svc.Head().Hash, y3.Hash)
	}
	snap, err := svc.QuerySnapshot(3)
	if err != nil {
		t.Fatal(err)
	}
	// alice 1000-40=960; y 25; z 15.
	bal := map[string]string{}
	for _, h := range snap.Tokens["TOK"].TopHolders {
		bal[h.Address] = h.Balance.String()
	}
	for addr, want := range map[string]string{"alice": "960", "y": "25", "z": "15"} {
		if bal[addr] != want {
			t.Fatalf("%s=%s want %s", addr, bal[addr], want)
		}
	}
	if _, ok := bal["x"]; ok {
		t.Fatal("old branch holder x must not survive recovered reorg")
	}
}
