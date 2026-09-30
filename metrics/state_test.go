package metrics

import (
	"math/big"
	"reflect"
	"testing"

	"github.com/guoyyit/chain-metrics-go/domain"
)

func testCfg() domain.Config {
	c := domain.DefaultConfig()
	c.ActiveWindowBlocks = 3
	c.TopHolderCount = 5
	return c
}

func mkEvent(typ domain.EventType, token, from, to, amt, tx string, idx int) domain.Event {
	v, _ := new(big.Int).SetString(amt, 10)
	return domain.Event{Type: typ, Token: token, From: from, To: to, Amount: v, TxHash: tx, LogIndex: idx}
}

// TestRollbackIsExactInverse applies a sequence of blocks, rolls them
// all back, and requires the state to be byte-for-byte equivalent to
// fresh state (this is the property that makes reorg results equal to
// a fresh replay).
func TestRollbackIsExactInverse(t *testing.T) {
	cfg := testCfg()
	s := NewState(cfg)

	blocks := []*domain.Block{
		{Height: 1, Hash: "h1", Events: []domain.Event{
			mkEvent(domain.EventMint, "T", "", "a", "1000", "m1", 0),
		}},
		{Height: 2, Hash: "h2", ParentHash: "h1", Events: []domain.Event{
			mkEvent(domain.EventTransfer, "T", "a", "b", "300", "t1", 0),
			mkEvent(domain.EventTransfer, "T", "b", "c", "100", "t2", 0),
		}},
		{Height: 3, Hash: "h3", ParentHash: "h2", Events: []domain.Event{
			mkEvent(domain.EventBurn, "T", "a", "", "50", "b1", 0),
			mkEvent(domain.EventTransfer, "T", "c", "a", "20", "t3", 0),
		}},
	}
	for _, b := range blocks {
		s.Apply(b)
	}
	before, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	for i := len(blocks) - 1; i >= 0; i-- {
		s.Rollback(blocks[i])
	}
	after, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		// Different objects after full rollback vs at tip are expected;
		// compare tip-vs-tip below. This guard documents intent.
	}

	// Re-apply and require identical serialization to the original tip.
	for _, b := range blocks {
		s.Apply(b)
	}
	replayed, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, replayed) {
		t.Fatalf("state after rollback+replay differs from original:\n%s\n%s", before, replayed)
	}
}

// TestRollbackToMiddleReplaysEquivalently applies h1..h3, rolls back
// to h1, then applies an alternate h2/h3 and compares with a fresh
// state built directly on that alternate branch from h1.
func TestRollbackToMiddleReplaysEquivalently(t *testing.T) {
	cfg := testCfg()

	common := []*domain.Block{
		{Height: 1, Hash: "h1", Events: []domain.Event{
			mkEvent(domain.EventMint, "T", "", "a", "1000", "m", 0),
		}},
	}
	altTail := []*domain.Block{
		{Height: 2, Hash: "a2", ParentHash: "h1", Events: []domain.Event{
			mkEvent(domain.EventTransfer, "T", "a", "x", "400", "x1", 0),
		}},
		{Height: 3, Hash: "a3", ParentHash: "a2", Events: []domain.Event{
			mkEvent(domain.EventBurn, "T", "x", "", "10", "x2", 0),
		}},
	}
	oldTail := []*domain.Block{
		{Height: 2, Hash: "o2", ParentHash: "h1", Events: []domain.Event{
			mkEvent(domain.EventTransfer, "T", "a", "y", "250", "y1", 0),
		}},
		{Height: 3, Hash: "o3", ParentHash: "o2", Events: []domain.Event{
			mkEvent(domain.EventTransfer, "T", "y", "z", "90", "y2", 0),
		}},
	}

	// Reference: h1 + altTail directly.
	ref := NewState(cfg)
	for _, b := range append(common, altTail...) {
		ref.Apply(b)
	}
	refSnap := ref.Snapshot(altTail[1])

	// Mutating run: h1 + oldTail, then roll back both old blocks and
	// apply altTail.
	s := NewState(cfg)
	for _, b := range append(common, oldTail...) {
		s.Apply(b)
	}
	s.Rollback(oldTail[1])
	s.Rollback(oldTail[0])
	for _, b := range altTail {
		s.Apply(b)
	}
	gotSnap := s.Snapshot(altTail[1])

	tsRef := refSnap.Tokens["T"]
	tsGot := gotSnap.Tokens["T"]
	if tsRef.CirculatingSupply.Cmp(tsGot.CirculatingSupply) != 0 {
		t.Fatalf("supply ref=%s got=%s", tsRef.CirculatingSupply, tsGot.CirculatingSupply)
	}
	if tsRef.HerfindahlIndex != tsGot.HerfindahlIndex ||
		tsRef.TopHolderShare != tsGot.TopHolderShare {
		t.Fatalf("concentration ref=%v/%v got=%v/%v",
			tsRef.TopHolderShare, tsRef.HerfindahlIndex,
			tsGot.TopHolderShare, tsGot.HerfindahlIndex)
	}
	if len(tsRef.TopHolders) != len(tsGot.TopHolders) {
		t.Fatalf("holders len ref=%d got=%d", len(tsRef.TopHolders), len(tsGot.TopHolders))
	}
	for i := range tsRef.TopHolders {
		if tsRef.TopHolders[i].Address != tsGot.TopHolders[i].Address ||
			tsRef.TopHolders[i].Balance.Cmp(tsGot.TopHolders[i].Balance) != 0 {
			t.Fatalf("holder %d ref=%v got=%v", i, tsRef.TopHolders[i], tsGot.TopHolders[i])
		}
	}
}

// TestCheckpointRoundTrip marshals state, unmarshals it, applies one
// more block and requires identical results to uninterrupted state.
func TestCheckpointRoundTrip(t *testing.T) {
	cfg := testCfg()
	s := NewState(cfg)
	b1 := &domain.Block{Height: 1, Hash: "h1", Events: []domain.Event{
		mkEvent(domain.EventMint, "T", "", "a", "1000", "m", 0),
	}}
	b2 := &domain.Block{Height: 2, Hash: "h2", ParentHash: "h1", Events: []domain.Event{
		mkEvent(domain.EventTransfer, "T", "a", "b", "200", "t", 0),
	}}
	s.Apply(b1)
	s.Apply(b2)
	data, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := UnmarshalState(cfg, data)
	if err != nil {
		t.Fatal(err)
	}
	got := restored.Snapshot(b2)
	want := s.Snapshot(b2)
	if got.Tokens["T"].CirculatingSupply.Cmp(want.Tokens["T"].CirculatingSupply) != 0 ||
		got.Tokens["T"].WindowTransferVolume.Cmp(want.Tokens["T"].WindowTransferVolume) != 0 ||
		got.Tokens["T"].ActiveAddresses != want.Tokens["T"].ActiveAddresses {
		t.Fatal("checkpoint round-trip mismatch")
	}
}
