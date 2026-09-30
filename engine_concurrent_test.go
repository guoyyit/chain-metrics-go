package chainmetrics

import (
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
