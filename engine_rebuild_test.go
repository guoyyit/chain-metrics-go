package chainmetrics

import "testing"

func TestIncrementalEqualsRebuild(t *testing.T) {
	const token = "TKN"
	var blocks []Block
	prev := ""
	addrs := []string{"A", "B", "C", "D", "E"}

	for n := uint64(1); n <= 40; n++ {
		hash := "h" + uintToStr(n)
		var events []Event
		switch {
		case n%7 == 0:
			events = append(events, ev(token, Burn, "A", "", 2, n, 0))
		case n%5 == 0:
			events = append(events, ev(token, Mint, "", addrs[n%5], 5, n, 0))
		default:
			from := addrs[n%5]
			to := addrs[(n+1)%5]
			events = append(events, ev(token, Transfer, from, to, 3, n, 0))
			if n%3 == 0 {
				events = append(events, ev(token, Transfer, to, from, 1, n, 1))
			}
		}
		if n == 1 {
			events = append([]Event{ev(token, Mint, "", "A", 1000, 1, 100)}, events...)
		}
		parent := prev
		blocks = append(blocks, block(n, hash, parent, events...))
		prev = hash
	}

	e, err := NewEngine(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for _, b := range blocks {
		mustPush(t, e, b)
	}

	firstMismatch, ok, err := e.VerifyAgainstRebuild(blocks)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("incremental vs rebuild mismatch at height %d", firstMismatch)
	}
}

func TestRollingWindowExpiry(t *testing.T) {
	cfg := testConfig()
	cfg.Metrics.ActiveWindowBlocks = 2
	cfg.Metrics.VolumeWindowBlocks = 2
	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	const token = "TKN"
	mustPush(t, e, block(1, "h1", "", ev(token, Mint, "", "A", 100, 1, 0)))
	mustPush(t, e, block(2, "h2", "h1", ev(token, Transfer, "A", "B", 10, 2, 0)))
	mustPush(t, e, block(3, "h3", "h2", ev(token, Transfer, "A", "C", 10, 3, 0)))

	m3 := metricAt(t, e, 3, token)
	if m3.ActiveAddresses != 3 {
		t.Fatalf("active=%d want 3", m3.ActiveAddresses)
	}
	if m3.TransferVolume.Int64() != 20 {
		t.Fatalf("volume=%s want 20", m3.TransferVolume)
	}

	mustPush(t, e, block(4, "h4", "h3", ev(token, Transfer, "A", "D", 5, 4, 0)))
	m4 := metricAt(t, e, 4, token)
	if m4.ActiveAddresses != 3 {
		t.Fatalf("active=%d want 3", m4.ActiveAddresses)
	}
	if m4.TransferVolume.Int64() != 15 {
		t.Fatalf("volume=%s want 15", m4.TransferVolume)
	}
}

func TestBoundedMemory(t *testing.T) {
	cfg := testConfig()
	cfg.HistoryBlocks = 8
	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	const token = "TKN"
	for n := uint64(1); n <= 200; n++ {
		parent := ""
		if n > 1 {
			parent = "h" + uintToStr(n-1)
		}
		mustPush(t, e, block(n, "h"+uintToStr(n), parent,
			ev(token, Mint, "", "A", 1, n, 0)))
	}

	if len(e.snapshots) > int(cfg.HistoryBlocks)+1 {
		t.Fatalf("snapshots grew unbounded: %d", len(e.snapshots))
	}
	if _, ok := e.QueryAt(1); ok {
		t.Fatal("height 1 snapshot should have been pruned")
	}
	if _, ok := e.QueryAt(200); !ok {
		t.Fatal("latest snapshot must remain queryable")
	}
	if e.chain.get("h1") != nil {
		t.Fatal("old block node should be pruned from the tree")
	}
}
