package chainmetrics

import "testing"

func TestDuplicateEvents(t *testing.T) {
	e, err := NewEngine(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	const token = "TKN"
	events := []Event{
		ev(token, Mint, "", "A", 100, 1, 0),
		ev(token, Transfer, "A", "B", 40, 1, 1),
	}
	// Push the same block twice (re-delivery) and with duplicated events.
	mustPush(t, e, block(1, "h1", "", events...))
	dup := append(append([]Event{}, events...), events...)
	mustPush(t, e, block(1, "h1", "", dup...))

	m := metricAt(t, e, 1, token)
	if m.CirculatingSupply.Int64() != 100 {
		t.Fatalf("supply=%s want 100", m.CirculatingSupply)
	}
	if m.TransferCount != 2 {
		t.Fatalf("transfers=%d want 2", m.TransferCount)
	}
	if got := balanceOf(m, "A"); got != 60 {
		t.Fatalf("A=%d want 60", got)
	}
	if got := balanceOf(m, "B"); got != 40 {
		t.Fatalf("B=%d want 40", got)
	}
}

func TestOutOfOrderBlocks(t *testing.T) {
	e, err := NewEngine(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	const token = "TKN"
	// Arrive block 3 before its ancestors; then fill 1, 2, and re-deliver 3.
	b3 := block(3, "h3", "h2", ev(token, Transfer, "A", "C", 10, 3, 0))
	b1 := block(1, "h1", "", ev(token, Mint, "", "A", 100, 1, 0))
	b2 := block(2, "h2", "h1", ev(token, Transfer, "A", "B", 20, 2, 0))

	mustPush(t, e, b3) // unknown parent: held back, nothing published
	if _, ok := e.QueryAt(3); ok {
		t.Fatal("block 3 must not publish before its ancestors")
	}
	mustPush(t, e, b1)
	mustPush(t, e, b2)
	mustPush(t, e, b3) // now links to the tip

	m := metricAt(t, e, 3, token)
	if m.CirculatingSupply.Int64() != 100 {
		t.Fatalf("supply=%s", m.CirculatingSupply)
	}
	if got := balanceOf(m, "A"); got != 70 {
		t.Fatalf("A=%d want 70", got)
	}
	if got := balanceOf(m, "B"); got != 20 {
		t.Fatalf("B=%d want 20", got)
	}
	if got := balanceOf(m, "C"); got != 10 {
		t.Fatalf("C=%d want 10", got)
	}
}

func TestLooseEventsMergeIntoBlock(t *testing.T) {
	e, err := NewEngine(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	const token = "TKN"
	// Events arrive first without a block, then the block arrives referencing
	// the same height. Indexes differ so both must count exactly once.
	if err := e.PushEvent(ev(token, Mint, "", "A", 100, 1, 0)); err != nil {
		t.Fatal(err)
	}
	if err := e.PushEvent(ev(token, Mint, "", "A", 100, 1, 0)); err != nil {
		t.Fatal(err)
	}
	if err := e.PushEvent(ev(token, Transfer, "A", "B", 30, 1, 1)); err != nil {
		t.Fatal(err)
	}
	mustPush(t, e, block(1, "h1", "",
		ev(token, Mint, "", "A", 100, 1, 0),     // duplicate loose event
		ev(token, Transfer, "A", "B", 30, 1, 1), // duplicate loose event
		ev(token, Transfer, "B", "C", 10, 1, 2), // new event
	))

	m := metricAt(t, e, 1, token)
	if m.CirculatingSupply.Int64() != 100 {
		t.Fatalf("supply=%s want 100", m.CirculatingSupply)
	}
	if got := balanceOf(m, "A"); got != 70 {
		t.Fatalf("A=%d want 70", got)
	}
	if got := balanceOf(m, "B"); got != 20 {
		t.Fatalf("B=%d want 20", got)
	}
	if got := balanceOf(m, "C"); got != 10 {
		t.Fatalf("C=%d want 10", got)
	}
	if m.TransferCount != 3 {
		t.Fatalf("transfers=%d want 3", m.TransferCount)
	}
}

func TestHigherBlockSnapshotUnaffectedByLateEvent(t *testing.T) {
	e, err := NewEngine(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	const token = "TKN"
	mustPush(t, e, block(1, "h1", "", ev(token, Mint, "", "A", 100, 1, 0)))
	mustPush(t, e, block(2, "h2", "h1", ev(token, Transfer, "A", "B", 10, 2, 0)))
	// A late duplicate for block 1 must not alter the block-2 snapshot.
	mustPush(t, e, block(1, "h1", "", ev(token, Mint, "", "A", 100, 1, 0)))

	m2 := metricAt(t, e, 2, token)
	if got := balanceOf(m2, "A"); got != 90 {
		t.Fatalf("A=%d want 90", got)
	}
	if got := balanceOf(m2, "B"); got != 10 {
		t.Fatalf("B=%d want 10", got)
	}
}
