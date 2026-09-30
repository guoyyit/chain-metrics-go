package chainmetrics

import (
	"path/filepath"
	"testing"
)

func recoveryBlocks() []Block {
	const token = "TKN"
	return []Block{
		block(1, "h1", "", ev(token, Mint, "", "A", 100, 1, 0)),
		block(2, "h2", "h1", ev(token, Transfer, "A", "B", 30, 2, 0)),
		block(3, "h3", "h2", ev(token, Burn, "A", "", 10, 3, 0)),
		block(4, "h4", "h3", ev(token, Transfer, "B", "C", 5, 4, 0)),
	}
}

func TestCrashRecovery(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	cfg.DataDir = filepath.Join(dir, "data")

	blocks := recoveryBlocks()

	// First run: ingest, then close (simulate clean shutdown / crash point).
	func() {
		e, err := NewEngine(cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range blocks[:3] {
			mustPush(t, e, b)
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	// Second run: recover and continue from the last published block.
	e2, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()

	m3 := metricAt(t, e2, 3, "TKN")
	if m3.CirculatingSupply.Int64() != 90 {
		t.Fatalf("after recovery supply=%s want 90", m3.CirculatingSupply)
	}
	if got := balanceOf(m3, "A"); got != 60 {
		t.Fatalf("A=%d want 60", got)
	}

	// Re-delivering recovered blocks must be idempotent.
	for _, b := range blocks[:3] {
		mustPush(t, e2, b)
	}
	m3Again := metricAt(t, e2, 3, "TKN")
	if m3Again.CirculatingSupply.Int64() != 90 || m3Again.TransferCount != 3 {
		t.Fatalf("re-apply changed state: supply=%s transfers=%d",
			m3Again.CirculatingSupply, m3Again.TransferCount)
	}

	// Continue with new blocks.
	mustPush(t, e2, blocks[3])
	m4 := metricAt(t, e2, 4, "TKN")
	if got := balanceOf(m4, "B"); got != 25 {
		t.Fatalf("B=%d want 25", got)
	}
	if got := balanceOf(m4, "C"); got != 5 {
		t.Fatalf("C=%d want 5", got)
	}

	// Final state must equal an uninterrupted run.
	ref, err := RebuildFromScratch(blocks, cfg.Metrics)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshotsEqual(mustSnap(t, e2, 4), ref[4]) {
		t.Fatalf("recovered engine diverges from uninterrupted rebuild at 4")
	}
}

func TestRecoveryAcrossReorg(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	cfg.DataDir = filepath.Join(dir, "data")

	const token = "TKN"
	func() {
		e, err := NewEngine(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer e.Close()
		mustPush(t, e, block(1, "h1", "", ev(token, Mint, "", "A", 100, 1, 0)))
		mustPush(t, e, block(2, "h2", "h1", ev(token, Transfer, "A", "B", 40, 2, 0)))
		mustPush(t, e, block(3, "h3", "h2", ev(token, Transfer, "A", "C", 10, 3, 0)))
		// Reorg to a longer fork before shutdown.
		mustPush(t, e, block(2, "h2p", "h1", ev(token, Transfer, "A", "D", 20, 2, 0)))
		mustPush(t, e, block(3, "h3p", "h2p", ev(token, Transfer, "A", "D", 20, 3, 0)))
		mustPush(t, e, block(4, "h4p", "h3p", ev(token, Burn, "D", "", 5, 4, 0)))
	}()

	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	fork := []Block{
		block(1, "h1", "", ev(token, Mint, "", "A", 100, 1, 0)),
		block(2, "h2p", "h1", ev(token, Transfer, "A", "D", 20, 2, 0)),
		block(3, "h3p", "h2p", ev(token, Transfer, "A", "D", 20, 3, 0)),
		block(4, "h4p", "h3p", ev(token, Burn, "D", "", 5, 4, 0)),
	}
	ref, _ := RebuildFromScratch(fork, cfg.Metrics)
	for _, h := range []uint64{1, 2, 3, 4} {
		got, ok := e.QueryAt(h)
		if !ok || !snapshotsEqual(got, ref[h]) {
			t.Fatalf("post-reorg recovery mismatch at %d", h)
		}
	}
}
