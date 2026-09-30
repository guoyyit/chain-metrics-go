package service

import (
	"sync"
	"testing"
	"time"

	"github.com/guoyyit/chain-metrics-go/domain"
)

// TestConcurrentQueryConsistency runs ingestion and queries in
// parallel. Every observed snapshot must be internally coherent: its
// height/hash must agree with every per-token slice, meaning no
// reader ever sees metrics assembled across different blocks.
func TestConcurrentQueryConsistency(t *testing.T) {
	cfg := testConfig(t)
	cfg.HistoryRetainBlocks = 100
	svc, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	cb := newChainBuilder()
	const n = 120
	blocks := make([]*domain.Block, 0, n)
	blocks = append(blocks, cb.next(1,
		mint("TOK", "alice", "1000000", "g0", 0),
		mint("NFT", "zoe", "10", "g1", 0),
	))
	for h := uint64(2); h <= n; h++ {
		evs := []domain.Event{
			transfer("TOK", pick(h, 0), pick(h, 1), "3", "t"+itoa(h), 0),
			transfer("NFT", pick(h, 2), pick(h, 0), "1", "n"+itoa(h), 0),
		}
		blocks = append(blocks, cb.next(h, evs...))
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Querier hammering both latest and historical heights.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap, err := svc.QuerySnapshot(0)
				if err != nil {
					continue
				}
				if err := checkCoherent(snap); err != nil {
					t.Errorf("incoherent snapshot: %v", err)
					return
				}
				h := snap.Height
				if h > 1 && seed%2 == 0 {
					old, err := svc.QuerySnapshot(h - 1)
					if err == nil {
						if err := checkCoherent(old); err != nil {
							t.Errorf("incoherent history snapshot: %v", err)
							return
						}
					}
				}
			}
		}(r)
	}

	// Ingester.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, b := range blocks {
			if err := svc.IngestBlock(b); err != nil {
				t.Errorf("ingest %d: %v", b.Height, err)
				return
			}
		}
	}()

	// Wait until ingest catches up, then stop queriers.
	for svc.Head().Height < n {
		time.Sleep(time.Millisecond)
	}
	close(stop)
	wg.Wait()

	if svc.Head().Height != n {
		t.Fatalf("final head = %d", svc.Head().Height)
	}
	final, err := svc.QuerySnapshot(n)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkCoherent(final); err != nil {
		t.Fatal(err)
	}
}

func pick(h uint64, k int) string {
	names := []string{"alice", "bob", "carol", "dave", "erin", "frank", "gina"}
	return names[(int(h)+k)%len(names)]
}

func checkCoherent(snap *domain.BlockSnapshot) error {
	for token, ts := range snap.Tokens {
		if ts.Height != snap.Height || ts.BlockHash != snap.BlockHash {
			return coherenceError{token: token, snap: snap.Height, got: ts.Height}
		}
	}
	return nil
}

type coherenceError struct {
	token string
	snap  uint64
	got   uint64
}

func (e coherenceError) Error() string {
	return "token " + e.token + " at height " + itoa(e.got) + " inside snapshot " + itoa(e.snap)
}
