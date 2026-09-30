package service

import "testing"

// TestWindowEviction verifies the rolling window covers exactly the
// last W blocks inclusive, and that old blocks are evicted so window
// volume/count/activity decay correctly.
func TestWindowEviction(t *testing.T) {
	cfg := testConfig(t)
	cfg.ActiveWindowBlocks = 3
	svc, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	cb := newChainBuilder()
	svc.IngestBlock(cb.next(1, mint("TOK", "alice", "100000", "g", 0)))
	// h2: 10, h3: 20, h4: 30, h5: 40, distinct counterpart addresses.
	peers := []string{"p2", "p3", "p4", "p5"}
	amts := []string{"10", "20", "30", "40"}
	for i, h := range []uint64{2, 3, 4, 5} {
		b := cb.next(h, transfer("TOK", "alice", peers[i], amts[i], "t"+itoa(h), 0))
		if err := svc.IngestBlock(b); err != nil {
			t.Fatal(err)
		}
	}

	// At h5 the window is h3+h4+h5 = 20+30+40 = 90, 3 transfers.
	s5, _ := svc.QuerySnapshot(5)
	t5 := s5.Tokens["TOK"]
	if t5.WindowTransferVolume.String() != "90" {
		t.Fatalf("window volume at 5 = %s, want 90", t5.WindowTransferVolume)
	}
	if t5.WindowTransferCount != 3 {
		t.Fatalf("window count at 5 = %d, want 3", t5.WindowTransferCount)
	}
	// Active: alice + p3,p4,p5 = 4 (p2 evicted).
	if t5.ActiveAddresses != 4 {
		t.Fatalf("active at 5 = %d, want 4", t5.ActiveAddresses)
	}

	// Historical consistency: at h3 the window is h2+h3 = 30.
	s3, _ := svc.QuerySnapshot(3)
	t3 := s3.Tokens["TOK"]
	if t3.WindowTransferVolume.String() != "30" {
		t.Fatalf("window volume at 3 = %s, want 30", t3.WindowTransferVolume)
	}
	if t3.ActiveAddresses != 3 {
		t.Fatalf("active at 3 = %d, want 3 (alice,p2,p3)", t3.ActiveAddresses)
	}
}

// TestBalanceTiersMoveWithBalance ensures tier counts move as an
// address's balance crosses bounds up and down.
func TestBalanceTiersMoveWithBalance(t *testing.T) {
	cfg := testConfig(t)
	cfg.ActiveWindowBlocks = 2
	svc, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	cb := newChainBuilder()
	// Mint a balance landing in tier 3 (10_000_000 <= b < 1e9).
	svc.IngestBlock(cb.next(1, mint("TOK", "alice", "50000000", "g", 0)))
	s1, _ := svc.QuerySnapshot(1)
	t1 := s1.Tokens["TOK"]
	var total1 uint64
	for _, c := range t1.BalanceTiers {
		total1 += c
	}
	if total1 != 1 || t1.BalanceTiers[3] != 1 {
		t.Fatalf("tiers after mint = %v, want single tier-3 holder", t1.BalanceTiers)
	}

	// Move most of it away; alice drops to 100 (tier 0), bob large.
	svc.IngestBlock(cb.next(2,
		transfer("TOK", "alice", "bob", "49999900", "big", 0),
	))
	s2, _ := svc.QuerySnapshot(2)
	t2 := s2.Tokens["TOK"]
	var total2 uint64
	for _, c := range t2.BalanceTiers {
		total2 += c
	}
	if total2 != 2 {
		t.Fatalf("total tiered holders = %d, want 2", total2)
	}
	if t2.BalanceTiers[3] != 1 || t2.BalanceTiers[0] != 1 {
		t.Fatalf("tiers after transfer = %v, want [1,0,0,1,0]", t2.BalanceTiers)
	}

	// Re-query confirms the tier movement is stable at the block.
	s2b, _ := svc.QuerySnapshot(2)
	if s2b.Tokens["TOK"].BalanceTiers[3] != 1 {
		t.Fatal("tier regression")
	}
}

// TestConcentrationMetrics checks top-K share and Herfindahl index
// against hand-computed values.
func TestConcentrationMetrics(t *testing.T) {
	cfg := testConfig(t)
	cfg.TopHolderCount = 2
	svc, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	cb := newChainBuilder()
	// Two holders: 750 and 250 -> HHI = .75^2+.25^2 = .625,
	// top-2 share = 1.0.
	svc.IngestBlock(cb.next(1, mint("TOK", "a", "750", "m1", 0)))
	svc.IngestBlock(cb.next(2, mint("TOK", "b", "250", "m2", 0)))
	s, _ := svc.QuerySnapshot(2)
	tok := s.Tokens["TOK"]
	if len(tok.TopHolders) != 2 {
		t.Fatalf("top holders = %d, want 2", len(tok.TopHolders))
	}
	if tok.TopHolders[0].Address != "a" || tok.TopHolders[0].Balance.String() != "750" {
		t.Fatalf("rank 1 = %v", tok.TopHolders[0])
	}
	if d := tok.HerfindahlIndex - 0.625; d > 1e-9 || d < -1e-9 {
		t.Fatalf("HHI = %v, want 0.625", tok.HerfindahlIndex)
	}
	if d := tok.TopHolderShare - 1.0; d > 1e-9 || d < -1e-9 {
		t.Fatalf("top share = %v, want 1.0", tok.TopHolderShare)
	}
}
