package service

import (
	"testing"

	"github.com/guoyyit/chain-metrics-go/domain"
	"github.com/guoyyit/chain-metrics-go/rebuild"
	"github.com/guoyyit/chain-metrics-go/store"
)

// TestReorg verifies that switching canonical chains at height 4
// produces exactly the state a fresh replay of the new branch yields,
// and that historical queries track the new chain.
func TestReorg(t *testing.T) {
	cfg := testConfig(t)

	svc, err := Open(cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer svc.Close()

	cb := newChainBuilder()
	b1 := cb.next(1, mint("TOK", "alice", "1000", "g", 0))
	b2 := cb.next(2, transfer("TOK", "alice", "bob", "100", "t2", 0))
	b3 := cb.next(3, transfer("TOK", "alice", "bob", "100", "t3", 0))

	// Old chain: 4a sends funds to oldA.
	old4 := cb.next(4, transfer("TOK", "alice", "oldA", "500", "t4old", 0))
	old5 := cb.next(5, transfer("TOK", "oldA", "oldB", "200", "t5old", 0))

	for _, b := range []*domain.Block{b1, b2, b3, old4, old5} {
		if err := svc.IngestBlock(b); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	if got := svc.Head().Height; got != 5 {
		t.Fatalf("head = %d", got)
	}
	pre, _ := svc.QuerySnapshot(5)
	preBal := map[string]string{}
	for _, h := range pre.Tokens["TOK"].TopHolders {
		preBal[h.Address] = h.Balance.String()
	}
	if preBal["oldA"] != "300" || preBal["oldB"] != "200" {
		t.Fatalf("pre-reorg balances wrong: %v", preBal)
	}

	// New chain forked at b3: 4b/5b/6b.
	new4 := cb.forkFrom(b3, 4, transfer("TOK", "alice", "newA", "700", "t4new", 0))
	new5 := cb.forkFrom(new4, 5, burn("TOK", "alice", "50", "t5new", 0))
	new6 := cb.forkFrom(new5, 6, transfer("TOK", "newA", "newB", "100", "t6new", 0))

	// Deliver the new branch: base last of the three blocks below head,
	// so the reorg fires when the competing branch becomes complete;
	// block 6 then appends through the pending buffer.
	for _, b := range []*domain.Block{new6, new5, new4} {
		if err := svc.IngestBlock(b); err != nil {
			// n6/n5 may legitimately wait for their parents once
			// later deliveries connect them; only new4 must succeed
			// here.
			if b == new4 {
				t.Fatalf("reorg base ingest: %v", err)
			}
		}
	}
	// Second pass: now that new4 is known, n5 and n6 connect.
	for _, b := range []*domain.Block{new5, new6} {
		if err := svc.IngestBlock(b); err != nil {
			t.Fatalf("branch connect ingest: %v", err)
		}
	}
	if svc.Head().Hash != new6.Hash {
		t.Fatalf("head = %s, want %s", svc.Head().Hash, new6.Hash)
	}

	got, err := svc.QuerySnapshot(6)
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	// Reference: full replay of the new canonical chain.
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	chain, err := rebuild.CanonicalChain(st, new6.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := rebuild.Verify(cfg, chain, got); err != nil {
		t.Fatalf("post-reorg metrics differ from fresh replay: %v", err)
	}

	// History at heights 1..3 must remain the common prefix; heights
	// 4..6 must carry new-chain hashes.
	for h, wantHash := range map[uint64]string{
		1: b1.Hash, 2: b2.Hash, 3: b3.Hash,
		4: new4.Hash, 5: new5.Hash, 6: new6.Hash,
	} {
		s, err := svc.QuerySnapshot(h)
		if err != nil {
			t.Fatalf("history %d: %v", h, err)
		}
		if s.BlockHash != wantHash {
			t.Fatalf("height %d hash = %s, want %s", h, s.BlockHash, wantHash)
		}
	}

	// Balances after new chain:
	// alice 1000-100-100-700-50 = 50; bob 200; newA 600; newB 100.
	tok := got.Tokens["TOK"]
	bal := map[string]string{}
	for _, h := range tok.TopHolders {
		bal[h.Address] = h.Balance.String()
	}
	for addr, want := range map[string]string{
		"alice": "50", "bob": "200", "newA": "600", "newB": "100",
	} {
		if bal[addr] != want {
			t.Fatalf("%s = %q, want %s", addr, bal[addr], want)
		}
	}
	if _, present := bal["oldA"]; present {
		t.Fatal("oldA must not survive reorg")
	}
	if tok.CirculatingSupply.String() != "950" {
		t.Fatalf("supply = %s, want 950", tok.CirculatingSupply)
	}
}

// TestReorgToAncestor rolls back several blocks to an ancestor that
// sits inside the configured window and replays a single replacement.
func TestReorgToAncestor(t *testing.T) {
	cfg := testConfig(t)
	svc, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	cb := newChainBuilder()
	b1 := cb.next(1, mint("TOK", "alice", "1000", "g", 0))
	b2 := cb.next(2, transfer("TOK", "alice", "bob", "10", "t2", 0))
	b3 := cb.next(3, transfer("TOK", "alice", "bob", "10", "t3", 0))
	b4 := cb.next(4, transfer("TOK", "alice", "bob", "10", "t4", 0))
	b5 := cb.next(5, transfer("TOK", "alice", "bob", "10", "t5", 0))
	for _, b := range []*domain.Block{b1, b2, b3, b4, b5} {
		if err := svc.IngestBlock(b); err != nil {
			t.Fatal(err)
		}
	}

	// Replace block 3 with a different event; extend to same height 5.
	r3 := cb.forkFrom(b2, 3, transfer("TOK", "alice", "carol", "200", "r3", 0))
	r4 := cb.forkFrom(r3, 4, transfer("TOK", "carol", "dave", "50", "r4", 0))
	r5 := cb.forkFrom(r4, 5, mint("TOK", "carol", "100", "r5", 0))
	for _, b := range []*domain.Block{r3} {
		if err := svc.IngestBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range []*domain.Block{r4, r5} {
		if err := svc.IngestBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	got, err := svc.QuerySnapshot(5)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := store.Open(cfg.DataDir)
	defer st.Close()
	chain, err := rebuild.CanonicalChain(st, r5.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := rebuild.Verify(cfg, chain, got); err != nil {
		t.Fatalf("ancestor reorg mismatch: %v", err)
	}
}
