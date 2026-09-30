package chainmetrics

import "testing"

func TestBasicMetrics(t *testing.T) {
	e, err := NewEngine(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	const token = "TKN"
	// Block 1: mint 100 to A.
	mustPush(t, e, block(1, "h1", "",
		ev(token, Mint, "", "A", 100, 1, 0),
	))
	// Block 2: A -> B 30; B -> A 10.
	mustPush(t, e, block(2, "h2", "h1",
		ev(token, Transfer, "A", "B", 30, 2, 0),
		ev(token, Transfer, "B", "A", 10, 2, 1),
	))
	// Block 3: burn 5 from A.
	mustPush(t, e, block(3, "h3", "h2",
		ev(token, Burn, "A", "", 5, 3, 0),
	))

	m1 := metricAt(t, e, 1, token)
	if m1.CirculatingSupply.Int64() != 100 || m1.HolderCount != 1 {
		t.Fatalf("block1 supply=%s holders=%d", m1.CirculatingSupply, m1.HolderCount)
	}
	if m1.ActiveAddresses != 1 || m1.TransferVolume.Int64() != 0 {
		t.Fatalf("block1 active=%d volume=%s", m1.ActiveAddresses, m1.TransferVolume)
	}

	m2 := metricAt(t, e, 2, token)
	if m2.CirculatingSupply.Int64() != 100 {
		t.Fatalf("block2 supply=%s", m2.CirculatingSupply)
	}
	if m2.HolderCount != 2 || m2.TransferCount != 3 {
		t.Fatalf("block2 holders=%d transfers=%d", m2.HolderCount, m2.TransferCount)
	}
	if m2.ActiveAddresses != 2 || m2.TransferVolume.Int64() != 40 {
		t.Fatalf("block2 active=%d volume=%s", m2.ActiveAddresses, m2.TransferVolume)
	}
	if got := balanceOf(m2, "A"); got != 80 {
		t.Fatalf("A balance=%d want 80", got)
	}
	if got := balanceOf(m2, "B"); got != 20 {
		t.Fatalf("B balance=%d want 20", got)
	}
	if m2.TierCounts["large"] != 0 || m2.TierCounts["small"] != 2 {
		t.Fatalf("tiers=%v", m2.TierCounts)
	}

	m3 := metricAt(t, e, 3, token)
	if m3.CirculatingSupply.Int64() != 95 {
		t.Fatalf("block3 supply=%s", m3.CirculatingSupply)
	}
	if got := balanceOf(m3, "A"); got != 75 {
		t.Fatalf("A balance=%d want 75", got)
	}
}

func balanceOf(m TokenMetrics, addr string) int64 {
	for _, h := range m.TopHoldings {
		if h.Address == addr {
			return h.Balance.Int64()
		}
	}
	return -1
}
