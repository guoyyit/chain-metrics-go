package service

import (
	"testing"

	"github.com/guoyyit/chain-metrics-go/domain"
	"github.com/guoyyit/chain-metrics-go/store"
)

func storeOpenForTest(cfg domain.Config) (*store.FileStore, error) {
	return store.Open(cfg.DataDir)
}

func buildRecoveryChain(t *testing.T, cb *chainBuilder, n uint64) []*domain.Block {
	t.Helper()
	chain := []*domain.Block{
		cb.next(1, mint("TOK", "alice", "1000000", "gen", 0)),
	}
	for h := uint64(2); h <= n; h++ {
		from := []string{"alice", "bob", "carol"}[(h)%3]
		to := []string{"bob", "carol", "dave"}[h%3]
		chain = append(chain, cb.next(h, transfer("TOK", from, to, "7", "tx"+itoa(h), 0)))
	}
	return chain
}

// TestCrashRecovery restarts the service twice from the same data
// directory and checks snapshots at the boundary match an
// uninterrupted in-memory run.
func TestCrashRecovery(t *testing.T) {
	cfg := testConfig(t)

	// Uninterrupted reference in a separate directory.
	refCfg := cfg
	refCfg.DataDir = cfg.DataDir + "-ref"
	ref, err := Open(refCfg)
	if err != nil {
		t.Fatal(err)
	}
	cbRef := newChainBuilder()
	refChain := buildRecoveryChain(t, cbRef, 20)
	for _, b := range refChain {
		if err := ref.IngestBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := ref.VerifyIncremental(); err != nil {
		t.Fatalf("ref verify: %v", err)
	}

	// Restarted run: same logical chain, crash after block 7.
	cb := newChainBuilder()
	chain := buildRecoveryChain(t, cb, 20)

	svc, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range chain[:7] {
		if err := svc.IngestBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen; must resume exactly at 7 with no double application.
	svc, err = Open(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if head := svc.Head(); head.Height != 7 {
		t.Fatalf("head after restart = %d, want 7", head.Height)
	}
	s7, err := svc.QuerySnapshot(7)
	if err != nil {
		t.Fatal(err)
	}
	r7, _ := ref.QuerySnapshot(7)
	if !snapshotsEqualForTest(s7, r7) {
		t.Fatal("snapshot at 7 differs after restart")
	}

	// Duplicate redelivery of already-published blocks is a no-op.
	for _, b := range chain[:7] {
		if err := svc.IngestBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	s7b, _ := svc.QuerySnapshot(7)
	if !snapshotsEqualForTest(s7, s7b) {
		t.Fatal("duplicate replay after restart changed state")
	}

	// Continue to 20, then restart again (checkpoint exists at 9/12/15/18).
	for _, b := range chain[7:] {
		if err := svc.IngestBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	svc, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if svc.Head().Height != 20 {
		t.Fatalf("head = %d, want 20", svc.Head().Height)
	}
	s20, err := svc.QuerySnapshot(20)
	if err != nil {
		t.Fatal(err)
	}
	r20, _ := ref.QuerySnapshot(20)
	if !snapshotsEqualForTest(s20, r20) {
		t.Fatal("snapshot at 20 differs after second restart")
	}
}

// TestCrashRecoveryNoCheckpoint disables checkpoints, forcing a full
// replay on restart, and verifies it yields the same state.
func TestCrashRecoveryNoCheckpoint(t *testing.T) {
	cfg := testConfig(t)
	cfg.CheckpointInterval = 0

	cb := newChainBuilder()
	chain := buildRecoveryChain(t, cb, 10)
	svc, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range chain {
		if err := svc.IngestBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := svc.QuerySnapshot(10)
	svc.Close()

	svc, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	after, err := svc.QuerySnapshot(10)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshotsEqualForTest(before, after) {
		t.Fatal("full-replay recovery differs")
	}
}

// TestCrashBeforeFirstHead simulates blocks synced to disk before the
// first head pointer commit. Reopen must recover and publish.
func TestCrashBeforeFirstHead(t *testing.T) {
	cfg := testConfig(t)

	st, err := storeOpenForTest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cb := newChainBuilder()
	b1 := cb.next(1, mint("TOK", "alice", "100", "g", 0))
	b2 := cb.next(2, transfer("TOK", "alice", "bob", "10", "t", 0))
	if err := st.SaveBlock(b1); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveBlock(b2); err != nil {
		t.Fatal(err)
	}
	// Deliberately no SaveHead: crash between block fsync and publish.
	st.Close()

	svc, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if svc.Head().Height != 2 {
		t.Fatalf("head after recover = %d, want 2", svc.Head().Height)
	}
	snap, err := svc.QuerySnapshot(2)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Tokens["TOK"].CirculatingSupply.String() != "100" {
		t.Fatalf("supply = %s, want 100", snap.Tokens["TOK"].CirculatingSupply)
	}
}

// snapshotsEqualForTest compares the metric-bearing fields of two
// snapshots via deep equality on serialized DTOs is overkill here:
// compare balances + counters explicitly.
func snapshotsEqualForTest(a, b *domain.BlockSnapshot) bool {
	if a == nil || b == nil || a.BlockHash != b.BlockHash || a.Height != b.Height {
		return false
	}
	if len(a.Tokens) != len(b.Tokens) {
		return false
	}
	for token, ta := range a.Tokens {
		tb := b.Tokens[token]
		if tb == nil {
			return false
		}
		if ta.CirculatingSupply.Cmp(tb.CirculatingSupply) != 0 ||
			ta.TotalMinted.Cmp(tb.TotalMinted) != 0 ||
			ta.TotalBurned.Cmp(tb.TotalBurned) != 0 ||
			ta.TransferCount != tb.TransferCount ||
			ta.TotalTransferVolume.Cmp(tb.TotalTransferVolume) != 0 ||
			ta.WindowTransferVolume.Cmp(tb.WindowTransferVolume) != 0 ||
			ta.WindowTransferCount != tb.WindowTransferCount ||
			ta.ActiveAddresses != tb.ActiveAddresses {
			return false
		}
		if len(ta.TopHolders) != len(tb.TopHolders) {
			return false
		}
		for i := range ta.TopHolders {
			if ta.TopHolders[i].Address != tb.TopHolders[i].Address ||
				ta.TopHolders[i].Balance.Cmp(tb.TopHolders[i].Balance) != 0 {
				return false
			}
		}
	}
	return true
}
