package chainmetrics

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestConcurrentReorgAndQuery repeatedly reorganizes while readers query every
// snapshot. Every returned snapshot must be internally consistent and must
// correspond to one fully published block (never a mix of branches, never
// empty).
func TestConcurrentReorgAndQuery(t *testing.T) {
	cfg := testConfig()
	cfg.HistoryBlocks = 40
	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	base := deepChain(20, "h")
	for _, b := range base {
		mustPush(t, e, b)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	reader := func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				snap, ok := e.QueryLatest()
				if !ok {
					continue
				}
				if snap.BlockHash == "" || len(snap.Tokens) == 0 {
					t.Errorf("empty/inconsistent snapshot during reorg")
					return
				}
				for tokenID, m := range snap.Tokens {
					if m.TokenID != tokenID || m.CirculatingSupply == nil {
						t.Errorf("malformed token metrics at %d", snap.BlockNumber)
						return
					}
				}
			}
		}
	}
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go reader()
	}

	// Alternate winning forks: each extends a recent block and overtakes the
	// current tip, forcing many reorganizations.
	for round := uint64(0); round < 12; round++ {
		snap, _ := e.QueryLatest()
		head := snap.BlockNumber
		// Fork at head-2 with a 4-block branch.
		forkHeight := head - 2
		parent := canonicalParentHash(e, forkHeight)
		prefix := byte('a' + round%26)
		tail := make([]Block, 0, 4)
		ph := parent
		for k := uint64(1); k <= 4; k++ {
			h := forkHeight + k
			name := string(prefix) + uintToStr(round) + "-" + uintToStr(h)
			b := block(h, name, ph,
				ev("TKN", Transfer, "genesis", name, 1, h, 1))
			tail = append(tail, b)
			ph = name
		}
		// Deliver tail-first to also exercise cascading under concurrency.
		for i := len(tail) - 1; i >= 0; i-- {
			if err := e.PushBlock(tail[i]); err != nil {
				t.Fatalf("round %d push: %v", round, err)
			}
		}
	}
	close(stop)
	wg.Wait()
}

func canonicalParentHash(e *Engine, height uint64) string {
	s, ok := e.QueryAt(height)
	if !ok {
		return ""
	}
	return s.BlockHash
}

// TestDurableDataBounded verifies WAL + envelope stay bounded as the chain
// grows far beyond HistoryBlocks.
func TestDurableDataBounded(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.HistoryBlocks = 4
	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for n := uint64(1); n <= 200; n++ {
		parent := ""
		if n > 1 {
			parent = "h" + uintToStr(n-1)
		}
		mustPush(t, e, block(n, "h"+uintToStr(n), parent,
			ev("TKN", Mint, "", "A", 1, n, 0)))
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	pbs, err := readTestWAL(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	// WAL keeps strictly-above-image blocks plus, transiently, one record at
	// the image height; the total is bounded by HistoryBlocks+1.
	if len(pbs) > int(cfg.HistoryBlocks)+1 {
		t.Fatalf("WAL has %d records, bound is %d", len(pbs), cfg.HistoryBlocks+1)
	}
	// Envelope embedded nodes are bounded to at most HistoryBlocks-1.
	raw, err := os.ReadFile(filepath.Join(cfg.DataDir, checkpointFile))
	if err != nil {
		t.Fatal(err)
	}
	cp, embedded, err := decodeCheckpoint(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(embedded) >= int(cfg.HistoryBlocks) {
		t.Fatalf("envelope embedded %d nodes, bound is %d", len(embedded), cfg.HistoryBlocks-1)
	}
	if cp.number+uint64(len(pbs)) < 200 {
		t.Fatalf("recoverable chain only reaches %d, want 200", cp.number+uint64(len(pbs)))
	}
}

func readTestWAL(dir string) ([]persistedBlock, error) {
	st, err := OpenStore(dir)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	return st.walBlocks()
}
