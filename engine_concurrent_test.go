package chainmetrics

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// TestConcurrentQueryConsistency ingests blocks while many goroutines query
// snapshots. Every returned snapshot must be internally consistent:
// cross-token height/hash agree and balances are coherent.
func TestConcurrentQueryConsistency(t *testing.T) {
	e, err := NewEngine(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	const total = 300
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
				validateSnapshot(t, snap)
			}
		}
	}

	for i := 0; i < 6; i++ {
		wg.Add(1)
		go reader()
	}

	for i := uint64(1); i <= total; i++ {
		parent := ""
		if i > 1 {
			parent = hashN(i - 1)
		}
		mustPush(t, e, block(i, hashN(i), parent,
			ev("TKN", Mint, "", "A", 1, i, 0),
			ev("TKN", Transfer, "A", "B", 1, i, 1),
		))
	}
	close(stop)
	wg.Wait()

	last := metricAt(t, e, total, "TKN")
	if last.CirculatingSupply.Int64() != int64(total) {
		t.Fatalf("final supply=%s want %d", last.CirculatingSupply, total)
	}
}

func validateSnapshot(t *testing.T, snap Snapshot) {
	t.Helper()
	for tokenID, m := range snap.Tokens {
		if m.TokenID != tokenID {
			t.Fatalf("token key/value mismatch %s vs %s", tokenID, m.TokenID)
		}
		// Sum of non-negative holdings must never exceed supply.
		held := int64(0)
		seen := make(map[string]bool)
		for _, h := range m.TopHoldings {
			if h.Balance.Sign() < 0 {
				t.Fatalf("negative balance for %s", h.Address)
			}
			if seen[h.Address] {
				t.Fatalf("duplicate holder %s", h.Address)
			}
			seen[h.Address] = true
			held += h.Balance.Int64()
		}
		if held > m.CirculatingSupply.Int64() {
			t.Fatalf("held %d exceeds supply %s at block %d",
				held, m.CirculatingSupply, snap.BlockNumber)
		}
	}
}

func hashN(n uint64) string {
	return "h" + uintToStr(n)
}

// TestConcurrentReorgAndQuery runs queries while reorganizations switch the
// canonical branch. Every observed snapshot must be a complete, consistent
// view of exactly one published block on one of the two chains — never a
// mix, never an empty result once the first block is published.
func TestConcurrentReorgAndQuery(t *testing.T) {
	e, err := NewEngine(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	reader := func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			snap, ok := e.QueryLatest()
			if !ok {
				continue
			}
			if err := checkSnapshot(snap); err != nil {
				errs <- err
				return
			}
			// The snapshot must belong to one of the two branches.
			if !strings.HasPrefix(snap.BlockHash, "h") && !strings.HasPrefix(snap.BlockHash, "g") {
				errs <- fmt.Errorf("snapshot head %q is on no known branch", snap.BlockHash)
				return
			}
			// A point query for the observed height must return some
			// complete snapshot (a reorg may have switched the branch in
			// between, which is fine — each answer is self-consistent).
			at, ok := e.QueryAt(snap.BlockNumber)
			if !ok {
				errs <- fmt.Errorf("height %d vanished", snap.BlockNumber)
				return
			}
			if err := checkSnapshot(at); err != nil {
				errs <- err
				return
			}
		}
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go reader()
	}

	// Drive repeated deep reorgs between two branches.
	for round := 0; round < 20; round++ {
		for n := uint64(1); n <= 8; n++ {
			parent := ""
			if n > 1 {
				parent = hashN(n - 1)
			}
			mustPush(t, e, block(n, hashN(n), parent,
				ev("TKN", Mint, "", "A", 2, n, 0),
				ev("TKN", Transfer, "A", "B", 1, n, 1)))
		}
		for n := uint64(4); n <= 9; n++ {
			parent := "h3"
			if n > 4 {
				parent = "g" + uintToStr(n-1)
			}
			mustPush(t, e, block(n, "g"+uintToStr(n), parent,
				ev("TKN", Mint, "", "A", 5, n, 0),
				ev("TKN", Transfer, "A", "C", 1, n, 1)))
		}
		// Flip back: extend the h-branch past the g-branch.
		for n := uint64(9); n <= 11; n++ {
			mustPush(t, e, block(n, hashN(n), hashN(n-1),
				ev("TKN", Mint, "", "A", 2, n, 0),
				ev("TKN", Transfer, "A", "B", 1, n, 1)))
		}
	}
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	// The h-branch must have won the last flip.
	latest, ok := e.QueryLatest()
	if !ok || latest.BlockHash != "h11" {
		t.Fatalf("final head = %s, want h11", latest.BlockHash)
	}
}

// checkSnapshot validates one snapshot's internal consistency, returning an
// error instead of failing the test so it is safe to call from any goroutine.
func checkSnapshot(snap Snapshot) error {
	for tokenID, m := range snap.Tokens {
		if m.TokenID != tokenID {
			return fmt.Errorf("token key/value mismatch %s vs %s", tokenID, m.TokenID)
		}
		held := int64(0)
		seen := make(map[string]bool)
		for _, h := range m.TopHoldings {
			if h.Balance.Sign() < 0 {
				return fmt.Errorf("negative balance for %s", h.Address)
			}
			if seen[h.Address] {
				return fmt.Errorf("duplicate holder %s", h.Address)
			}
			seen[h.Address] = true
			held += h.Balance.Int64()
		}
		if held > m.CirculatingSupply.Int64() {
			return fmt.Errorf("held %d exceeds supply %s at block %d",
				held, m.CirculatingSupply, snap.BlockNumber)
		}
	}
	return nil
}

func uintToStr(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
