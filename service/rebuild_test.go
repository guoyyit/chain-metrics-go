package service

import (
	"testing"

	"github.com/guoyyit/chain-metrics-go/domain"
)

// TestVerifyIncremental builds a chain with mints, transfers and
// burns and requires the incremental snapshot to exactly equal the
// full-replay reference snapshot at the tip.
func TestVerifyIncremental(t *testing.T) {
	cfg := testConfig(t)
	svc, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	cb := newChainBuilder()
	blocks := []*domain.Block{
		cb.next(1,
			mint("TOK", "alice", "5000", "m1", 0),
			mint("COIN", "zoe", "200", "m2", 0),
		),
		cb.next(2,
			transfer("TOK", "alice", "bob", "1200", "t1", 0),
			transfer("TOK", "bob", "carol", "300", "t2", 0),
		),
		cb.next(3,
			burn("TOK", "alice", "250", "b1", 0),
			transfer("COIN", "zoe", "bob", "90", "c1", 0),
		),
		cb.next(4,
			transfer("TOK", "carol", "alice", "150", "t3", 0),
			transfer("TOK", "alice", "dave", "1000", "t4", 0),
			burn("COIN", "zoe", "10", "b2", 0),
		),
	}
	for _, b := range blocks {
		if err := svc.IngestBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.VerifyIncremental(); err != nil {
		t.Fatalf("incremental vs full replay mismatch: %v", err)
	}

	// Compare concrete metric values against hand computation.
	snap, _ := svc.QuerySnapshot(4)
	tok := snap.Tokens["TOK"]
	// mint 5000; burn 250 -> supply 4750.
	if tok.CirculatingSupply.String() != "4750" {
		t.Fatalf("supply %s", tok.CirculatingSupply)
	}
	// transfers: 1200+300+150+1000 = 2650
	if tok.TotalTransferVolume.String() != "2650" || tok.TransferCount != 4 {
		t.Fatalf("volume %s count %d", tok.TotalTransferVolume, tok.TransferCount)
	}
}

// TestVerifyAfterReorg runs verification following a reorganization,
// ensuring the full-replay reference matches the reorged state.
func TestVerifyAfterReorg(t *testing.T) {
	cfg := testConfig(t)
	svc, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	cb := newChainBuilder()
	b1 := cb.next(1, mint("TOK", "alice", "100", "g", 0))
	b2 := cb.next(2, transfer("TOK", "alice", "bob", "10", "x", 0))
	b3 := cb.next(3, transfer("TOK", "alice", "bob", "10", "y", 0))
	for _, b := range []*domain.Block{b1, b2, b3} {
		if err := svc.IngestBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	r2 := cb.forkFrom(b1, 2, transfer("TOK", "alice", "carol", "30", "r2", 0))
	r3 := cb.forkFrom(r2, 3, burn("TOK", "alice", "5", "r3", 0))
	if err := svc.IngestBlock(r2); err != nil {
		t.Fatal(err)
	}
	if err := svc.IngestBlock(r3); err != nil {
		t.Fatal(err)
	}
	if err := svc.VerifyIncremental(); err != nil {
		t.Fatalf("post-reorg verify: %v", err)
	}
}
