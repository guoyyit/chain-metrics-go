package service

import (
	"testing"

	"github.com/guoyyit/chain-metrics-go/domain"
)

// TestBasicMetricsAndQuery checks supply, volume, window activity and
// tier counts for a simple mint/transfer/burn sequence.
func TestBasicMetricsAndQuery(t *testing.T) {
	svc, err := Open(testConfig(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer svc.Close()

	cb := newChainBuilder()
	b1 := cb.next(1,
		mint("TOK", "alice", "1000", "tx1", 0),
	)
	b2 := cb.next(2,
		transfer("TOK", "alice", "bob", "300", "tx2", 0),
		transfer("TOK", "alice", "carol", "200", "tx3", 0),
	)
	b3 := cb.next(3,
		transfer("TOK", "bob", "carol", "100", "tx4", 0),
		burn("TOK", "alice", "50", "tx5", 0),
	)
	for _, b := range []*domain.Block{b1, b2, b3} {
		if err := svc.IngestBlock(b); err != nil {
			t.Fatalf("ingest %d: %v", b.Height, err)
		}
	}

	head := svc.Head()
	if head.Height != 3 {
		t.Fatalf("head height = %d, want 3", head.Height)
	}

	snap, err := svc.QuerySnapshot(0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	tok := snap.Tokens["TOK"]
	if tok == nil {
		t.Fatal("TOK snapshot missing")
	}
	if got := tok.CirculatingSupply.String(); got != "950" {
		t.Fatalf("circulating = %s, want 950", got)
	}
	if tok.TotalMinted.String() != "1000" || tok.TotalBurned.String() != "50" {
		t.Fatalf("minted/burned = %s/%s", tok.TotalMinted, tok.TotalBurned)
	}
	if tok.TransferCount != 3 || tok.TotalTransferVolume.String() != "600" {
		t.Fatalf("transfers = %d/%s, want 3/600", tok.TransferCount, tok.TotalTransferVolume)
	}
	// Window is the last 4 blocks; all 3 transfers are inside.
	if tok.WindowTransferVolume.String() != "600" || tok.WindowTransferCount != 3 {
		t.Fatalf("window transfers = %s/%d, want 600/3", tok.WindowTransferVolume, tok.WindowTransferCount)
	}
	// Unique active addresses: alice, bob, carol.
	if tok.ActiveAddresses != 3 {
		t.Fatalf("active = %d, want 3", tok.ActiveAddresses)
	}
	// Final balances: alice 450, bob 200, carol 300.
	balances := map[string]string{}
	for _, h := range tok.TopHolders {
		balances[h.Address] = h.Balance.String()
	}
	for addr, want := range map[string]string{"alice": "450", "bob": "200", "carol": "300"} {
		if balances[addr] != want {
			t.Fatalf("balance %s = %q, want %s", addr, balances[addr], want)
		}
	}
	if len(tok.BalanceTiers) == 0 {
		t.Fatal("tiers missing")
	}
}

// TestOutOfOrderAndDuplicate delivers blocks in scrambled order and
// repeatedly, and requires the same result as in-order delivery.
func TestOutOfOrderAndDuplicate(t *testing.T) {
	svc, err := Open(testConfig(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer svc.Close()

	cb := newChainBuilder()
	var chain []*domain.Block
	prev := ""
	_ = prev
	for h := uint64(1); h <= 8; h++ {
		b := cb.next(h,
			transfer("TOK", "alice", "bob", "10", "tx-h"+itoa(h), 0),
		)
		chain = append(chain, b)
	}
	// Seed alice first.
	gen := cb.next(1, mint("TOK", "alice", "1000000", "gen", 0))
	// Rebuild chain: the mint must be block 1; recreate blocks 2..8
	// with the correct parent linkage instead.
	cb = newChainBuilder()
	chain = nil
	chain = append(chain, cb.next(1, mint("TOK", "alice", "1000000", "gen", 0)))
	for h := uint64(2); h <= 8; h++ {
		chain = append(chain, cb.next(h, transfer("TOK", "alice", "bob", "10", "tx-h"+itoa(h), 0)))
	}
	_ = gen

	// Scrambled order: 4,2,7,..., waiting on parents.
	order := []uint64{4, 2, 7, 1, 3, 6, 5, 8}
	for _, h := range order {
		if err := svc.IngestBlock(chain[h-1]); err != nil {
			t.Fatalf("ingest %d: %v", h, err)
		}
	}
	// Duplicate every block, including redelivery while head already
	// advanced past them.
	for _, b := range chain {
		if err := svc.IngestBlock(b); err != nil {
			t.Fatalf("duplicate %d: %v", b.Height, err)
		}
	}
	if svc.Head().Height != 8 {
		t.Fatalf("head = %d, want 8", svc.Head().Height)
	}
	snap, err := svc.QuerySnapshot(8)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	tok := snap.Tokens["TOK"]
	// 7 transfers of 10 after the 1,000,000 mint.
	if tok.TransferCount != 7 || tok.TotalTransferVolume.String() != "70" {
		t.Fatalf("transfers = %d/%s, want 7/70", tok.TransferCount, tok.TotalTransferVolume)
	}
	if tok.CirculatingSupply.String() != "1000000" {
		t.Fatalf("supply = %s", tok.CirculatingSupply)
	}
}
