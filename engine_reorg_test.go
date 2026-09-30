package chainmetrics

import "testing"

// canonical: h1 -> h2 -> h3
// fork:      h1 -> h2' -> h3'
func buildForkChains() (canonical, forkA []Block) {
	const token = "TKN"
	canonical = []Block{
		block(1, "h1", "", ev(token, Mint, "", "A", 100, 1, 0)),
		block(2, "h2", "h1", ev(token, Transfer, "A", "B", 40, 2, 0)),
		block(3, "h3", "h2", ev(token, Transfer, "A", "C", 30, 3, 0)),
	}
	forkA = []Block{
		block(1, "h1", "", ev(token, Mint, "", "A", 100, 1, 0)),
		block(2, "h2p", "h1", ev(token, Transfer, "A", "D", 50, 2, 0)),
		block(3, "h3p", "h2p", ev(token, Burn, "A", "", 10, 3, 0)),
	}
	return canonical, forkA
}

func TestReorganization(t *testing.T) {
	e, err := NewEngine(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	canonical, forkA := buildForkChains()
	for _, b := range canonical {
		mustPush(t, e, b)
	}
	// Push the fork: h2p shares parent h1, h3p follows.
	mustPush(t, e, forkA[1])
	mustPush(t, e, forkA[2])
	mustPush(t, e, block(4, "h4p", "h3p",
		ev("TKN", Transfer, "D", "E", 5, 4, 0)))

	full := append(append([]Block{}, forkA...),
		block(4, "h4p", "h3p", ev("TKN", Transfer, "D", "E", 5, 4, 0)))

	// Reference: fresh replay of the fork branch.
	ref, err := RebuildFromScratch(full, testConfig().Metrics)
	if err != nil {
		t.Fatal(err)
	}

	for _, height := range []uint64{1, 2, 3, 4} {
		got, ok := e.QueryAt(height)
		if !ok {
			t.Fatalf("missing snapshot at %d", height)
		}
		if !snapshotsEqual(got, ref[height]) {
			t.Fatalf("height %d mismatch:\n got=%+v\nwant=%+v", height, got, ref[height])
		}
	}

	m3 := metricAt(t, e, 3, "TKN")
	if m3.CirculatingSupply.Int64() != 90 {
		t.Fatalf("supply=%s want 90 (50 to D, 10 burned from A)", m3.CirculatingSupply)
	}
	if got := balanceOf(m3, "A"); got != 40 {
		t.Fatalf("A=%d want 40", got)
	}
	if got := balanceOf(m3, "D"); got != 50 {
		t.Fatalf("D=%d want 50", got)
	}
	if balanceOf(m3, "B") != -1 {
		t.Fatalf("B must disappear after reorg, got %d", balanceOf(m3, "B"))
	}

	m4 := metricAt(t, e, 4, "TKN")
	if got := balanceOf(m4, "D"); got != 45 {
		t.Fatalf("D=%d want 45", got)
	}
	if got := balanceOf(m4, "E"); got != 5 {
		t.Fatalf("E=%d want 5", got)
	}
}

func TestReorgThenExtend(t *testing.T) {
	e, err := NewEngine(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	canonical, forkA := buildForkChains()
	for _, b := range canonical {
		mustPush(t, e, b)
	}
	mustPush(t, e, forkA[1])
	mustPush(t, e, forkA[2])
	// Extend the new canonical tip.
	b4 := block(4, "h4p", "h3p",
		ev("TKN", Transfer, "D", "E", 5, 4, 0))
	mustPush(t, e, b4)

	m4 := metricAt(t, e, 4, "TKN")
	if got := balanceOf(m4, "D"); got != 45 {
		t.Fatalf("D=%d want 45", got)
	}
	if got := balanceOf(m4, "E"); got != 5 {
		t.Fatalf("E=%d want 5", got)
	}

	ref, _ := RebuildFromScratch(append(append([]Block{}, forkA...), b4), testConfig().Metrics)
	if !snapshotsEqual(mustSnap(t, e, 4), ref[4]) {
		t.Fatalf("height 4 mismatch after reorg+extend")
	}
}

func mustSnap(t *testing.T, e *Engine, h uint64) Snapshot {
	t.Helper()
	s, ok := e.QueryAt(h)
	if !ok {
		t.Fatalf("snapshot %d", h)
	}
	return s
}
