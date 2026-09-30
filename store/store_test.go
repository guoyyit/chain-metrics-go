package store

import (
	"errors"
	"math/big"
	"path/filepath"
	"testing"

	"github.com/guoyyit/chain-metrics-go/domain"
	"github.com/guoyyit/chain-metrics-go/metrics"
)

func sampleBlock(h uint64, parent, hash string) *domain.Block {
	return &domain.Block{
		Height:     h,
		Hash:       hash,
		ParentHash: parent,
		Timestamp:  int64(h),
		Events: []domain.Event{
			{
				Type:     domain.EventTransfer,
				Token:    "TOK",
				From:     "a",
				To:       "b",
				Amount:   big.NewInt(int64(h)),
				TxHash:   "tx" + hash,
				LogIndex: 0,
			},
		},
	}
}

func TestBlockIdempotentPersistence(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "d"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	b := sampleBlock(7, "p7", "hash7")
	if err := st.SaveBlock(b); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveBlock(b); err != nil {
		t.Fatalf("duplicate save: %v", err)
	}
	if !st.HasBlockAt(7, "hash7") {
		t.Fatal("HasBlockAt false after save")
	}
	got, err := st.LoadBlockAt(7, "hash7")
	if err != nil {
		t.Fatal(err)
	}
	if got.Events[0].Amount.Cmp(big.NewInt(7)) != 0 {
		t.Fatalf("amount round-trip = %s", got.Events[0].Amount)
	}
	if _, err := st.LoadBlockAt(7, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing block err = %v", err)
	}
}

func TestHeadAndCheckpointAtomic(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "d"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := st.LoadHead(); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("initial head err = %v", err)
	}
	if err := st.SaveHead(domain.HeadPointer{Height: 11, Hash: "h11"}); err != nil {
		t.Fatal(err)
	}
	head, err := st.LoadHead()
	if err != nil || head.Height != 11 || head.Hash != "h11" {
		t.Fatalf("head round-trip: %+v err=%v", head, err)
	}

	cp := &metrics.Checkpoint{Height: 9, Hash: "h9", Data: []byte(`{"ok":true}`)}
	if err := st.SaveCheckpoint(cp); err != nil {
		t.Fatal(err)
	}
	got, err := st.LatestCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if got.Height != 9 || string(got.Data) != `{"ok":true}` {
		t.Fatalf("checkpoint round-trip = %+v", got)
	}
}

func TestSnapshotArchivePruneAndReorgRewrite(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "d"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	tokSnap := func(h uint64) *domain.BlockSnapshot {
		return &domain.BlockSnapshot{
			Height:    h,
			BlockHash: "hash" + string(rune('0'+h)),
			Timestamp: int64(h),
			Tokens: map[string]*domain.TokenSnapshot{
				"TOK": {Token: "TOK", Height: h, CirculatingSupply: big.NewInt(int64(h))},
			},
		}
	}
	for h := uint64(1); h <= 5; h++ {
		if err := st.SaveSnapshot(tokSnap(h)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.SnapshotAtHeight(3); err != nil {
		t.Fatalf("read 3: %v", err)
	}

	// Prune below 4.
	if err := st.PruneSnapshots(4); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SnapshotAtHeight(3); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("pruned read err = %v", err)
	}
	if s, err := st.SnapshotAtHeight(4); err != nil || s.Height != 4 {
		t.Fatalf("retained read: %v %v", s, err)
	}

	// Reorg rewrite at ancestor height 4 with a replacement height 5.
	branch := []*domain.BlockSnapshot{
		{Height: 5, BlockHash: "new5", Tokens: map[string]*domain.TokenSnapshot{
			"TOK": {Token: "TOK", Height: 5, BlockHash: "new5", CirculatingSupply: big.NewInt(55)},
		}},
	}
	if err := st.RewriteArchiveDuringReorg(4, branch); err != nil {
		t.Fatal(err)
	}
	s5, err := st.SnapshotAtHeight(5)
	if err != nil {
		t.Fatal(err)
	}
	if s5.BlockHash != "new5" || s5.Tokens["TOK"].CirculatingSupply.Int64() != 55 {
		t.Fatalf("reorg rewrite wrong snapshot: %+v", s5)
	}
	s4, err := st.SnapshotAtHeight(4)
	if err != nil || s4.BlockHash == "new5" {
		t.Fatalf("ancestor must be retained: %+v err=%v", s4, err)
	}
}
