// Package rebuild provides the full-replay path: it reconstructs
// metrics from the stored block log without any incremental shortcuts.
// It is both the reference implementation used to verify incremental
// results and the bootstrap path for crash recovery.
package rebuild

import (
	"reflect"
	"sort"

	"github.com/guoyyit/chain-metrics-go/domain"
	"github.com/guoyyit/chain-metrics-go/metrics"
	"github.com/guoyyit/chain-metrics-go/store"
)

// CanonicalChain returns the blocks of the chain ending at tipHash in
// ascending height order. It indexes stored blocks by (height, hash),
// walks parent pointers from the tip and stops at the earliest stored
// ancestor.
func CanonicalChain(st store.Store, tipHash string) ([]*domain.Block, error) {
	byHash := make(map[string]*domain.Block)
	if err := st.StreamBlocks(func(b *domain.Block) error {
		byHash[b.Hash] = b
		return nil
	}); err != nil {
		return nil, err
	}

	rev := make([]*domain.Block, 0)
	hash := tipHash
	for hash != "" {
		b, ok := byHash[hash]
		if !ok {
			break
		}
		rev = append(rev, b)
		hash = b.ParentHash
	}
	chain := make([]*domain.Block, len(rev))
	for i, b := range rev {
		chain[len(rev)-1-i] = b
	}
	return chain, nil
}

// FullReplay applies the canonical chain into a fresh metrics.State.
// The result is by construction the ground truth that incremental
// maintenance must match.
func FullReplay(cfg domain.Config, st store.Store, tip domain.HeadPointer) (*metrics.State, error) {
	chain, err := CanonicalChain(st, tip.Hash)
	if err != nil {
		return nil, err
	}
	state := metrics.NewState(cfg)
	for _, b := range chain {
		if b.Height > tip.Height {
			break
		}
		state.Apply(b)
	}
	return state, nil
}

// Verify builds the reference state at the current tip and compares
// the snapshot produced by a full replay with the incrementally
// produced snapshot passed in. Returns nil on an exact match.
func Verify(cfg domain.Config, chain []*domain.Block, got *domain.BlockSnapshot) error {
	state := metrics.NewState(cfg)
	var tip *domain.Block
	for _, b := range chain {
		if got != nil && b.Height > got.Height {
			break
		}
		state.Apply(b)
		tip = b
	}
	if tip == nil || got == nil {
		return domain.ErrNotFound
	}
	want := state.Snapshot(tip)
	normalize(want)
	normalize(got)
	if !reflect.DeepEqual(want, got) {
		return ErrMismatch
	}
	return nil
}

func normalize(snap *domain.BlockSnapshot) {
	for _, t := range snap.Tokens {
		sort.Slice(t.TopHolders, func(i, j int) bool {
			return t.TopHolders[i].Address < t.TopHolders[j].Address
		})
	}
}

// typed sentinel
type mismatchError struct{}

func (mismatchError) Error() string { return "incremental metrics do not match full replay" }

// ErrMismatch is returned by Verify on any difference.
var ErrMismatch error = mismatchError{}
