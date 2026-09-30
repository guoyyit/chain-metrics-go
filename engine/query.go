package engine

import (
	"math/big"

	"github.com/guoyyit/chain-metrics-go/domain"
)

// SnapshotAtHeight returns the fully consistent snapshot published at
// height. height 0 means the latest published block. Heights in the
// retained ring are served from memory; older retained heights are
// streamed from the on-disk archive.
func (e *Engine) SnapshotAtHeight(height uint64) (*domain.BlockSnapshot, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if e.head.Height == 0 {
		return nil, domain.ErrHeightNotYetPublished
	}
	if height == 0 {
		height = e.head.Height
	}
	if height > e.head.Height {
		return nil, domain.ErrHeightNotYetPublished
	}
	retain := e.cfg.HistoryRetainBlocks
	if retain > 0 && e.head.Height >= retain && height < e.head.Height-retain+1 {
		return nil, domain.ErrHeightPruned
	}
	if snap, ok := e.history.get(height); ok {
		return cloneSnapshot(snap), nil
	}
	// Below the in-memory ring floor. Serve from durable archive if
	// still retained there; otherwise the height is pruned.
	snap, err := e.st.SnapshotAtHeight(height)
	if err != nil {
		return nil, err
	}
	return snap, nil
}

// TokenAtHeight returns one token's slice of a block snapshot.
func (e *Engine) TokenAtHeight(height uint64, token string) (*domain.TokenSnapshot, error) {
	snap, err := e.SnapshotAtHeight(height)
	if err != nil {
		return nil, err
	}
	ts, ok := snap.Tokens[token]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return ts, nil
}

// snapshotRing is a fixed-capacity ring holding the most recent
// published snapshots. Capacity is bounded by HistoryRetainBlocks, so
// ring memory cannot grow with chain length.
type snapshotRing struct {
	capacity uint64
	base     uint64 // height of slots[0] when len>0
	head     uint64 // highest stored height
	slots    []*domain.BlockSnapshot
}

// archiveFloor tracks the lowest height the on-disk archive must
// retain. Physical rewrites are batched so steady-state ingestion is
// append-only and never O(chain length) per block.

// cloneSnapshot returns a deep copy so callers can never mutate a
// published snapshot (concurrent readers would otherwise risk torn
// state across queries).
func cloneSnapshot(s *domain.BlockSnapshot) *domain.BlockSnapshot {
	cp := &domain.BlockSnapshot{
		Height:    s.Height,
		BlockHash: s.BlockHash,
		Timestamp: s.Timestamp,
		Tokens:    make(map[string]*domain.TokenSnapshot, len(s.Tokens)),
	}
	for token, t := range s.Tokens {
		ct := *t
		ct.CirculatingSupply = new(big.Int).Set(t.CirculatingSupply)
		ct.TotalMinted = new(big.Int).Set(t.TotalMinted)
		ct.TotalBurned = new(big.Int).Set(t.TotalBurned)
		ct.TotalTransferVolume = new(big.Int).Set(t.TotalTransferVolume)
		ct.WindowTransferVolume = new(big.Int).Set(t.WindowTransferVolume)
		ct.BalanceTiers = append([]uint64(nil), t.BalanceTiers...)
		ct.TopHolders = make([]domain.HolderBalance, len(t.TopHolders))
		for i, h := range t.TopHolders {
			ct.TopHolders[i] = domain.HolderBalance{
				Address: h.Address,
				Balance: new(big.Int).Set(h.Balance),
			}
		}
		cp.Tokens[token] = &ct
	}
	return cp
}

func newSnapshotRing(capacity uint64) *snapshotRing {
	if capacity == 0 {
		capacity = 1
	}
	return &snapshotRing{capacity: capacity}
}

func (r *snapshotRing) index(height uint64) int {
	return int((height - r.base) % r.capacity)
}

func (r *snapshotRing) put(snap *domain.BlockSnapshot) {
	if len(r.slots) == 0 {
		r.base = snap.Height
		r.head = snap.Height
		r.slots = make([]*domain.BlockSnapshot, r.capacity)
		r.slots[0] = snap
		return
	}
	if snap.Height <= r.head {
		// Reorg replay overwrites earlier positions.
		if snap.Height < r.base {
			return
		}
		r.slots[r.index(snap.Height)] = snap
		if snap.Height > r.head {
			r.head = snap.Height
		}
		return
	}
	r.head = snap.Height
	r.slots[r.index(snap.Height)] = snap
	// Recompute the base and evict the slot that wrapped.
	if r.head-r.base >= r.capacity {
		r.base = r.head - r.capacity + 1
	}
}

func (r *snapshotRing) get(height uint64) (*domain.BlockSnapshot, bool) {
	if len(r.slots) == 0 || height < r.base || height > r.head {
		return nil, false
	}
	snap := r.slots[r.index(height)]
	if snap == nil || snap.Height != height {
		return nil, false
	}
	return snap, true
}

// floor returns the lowest queryable height currently held.
func (r *snapshotRing) floor() uint64 {
	if len(r.slots) == 0 {
		return 0
	}
	return r.base
}

// truncateAbove drops entries with height above the given height and
// resets the ring around it, used during fork rollback.
func (r *snapshotRing) truncateAbove(height uint64) {
	if len(r.slots) == 0 {
		return
	}
	for h := r.head; h > height && h >= r.base; h-- {
		r.slots[r.index(h)] = nil
	}
	if height < r.base {
		r.slots = make([]*domain.BlockSnapshot, r.capacity)
		r.base = 0
		r.head = 0
		return
	}
	r.head = height
}
