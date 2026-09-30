package metrics

import (
	"math/big"

	"github.com/guoyyit/chain-metrics-go/domain"
)

// blockTransferAgg is one token's transfer contribution at one block.
// Only plain transfers count toward rolling windows; mints and burns
// are excluded.
type blockTransferAgg struct {
	volume *big.Int
	count  uint64
	addrs  map[string]struct{}
}

func newBlockTransferAgg() blockTransferAgg {
	return blockTransferAgg{volume: new(big.Int), addrs: make(map[string]struct{})}
}

func (a blockTransferAgg) clone() blockTransferAgg {
	cp := blockTransferAgg{
		volume: new(big.Int).Set(a.volume),
		count:  a.count,
		addrs:  make(map[string]struct{}, len(a.addrs)),
	}
	for addr := range a.addrs {
		cp.addrs[addr] = struct{}{}
	}
	return cp
}

// windowView is the aggregated window contribution attached to a
// snapshot: transfer volume/count and the unique active address count
// over the retained blocks.
type windowView struct {
	volume  *big.Int
	count   uint64
	address uint64
}

// tokenWindow retains the last W per-block aggregates for one token.
// heights is strictly ascending; all retained memory is O(W) per
// active token, independent of total chain length.
type tokenWindow struct {
	heights []uint64
	entries []blockTransferAgg
}

// rollingWindow maintains rolling transfer/activity windows across
// tokens. The window covers W blocks inclusive of the current block:
// heights in [head-W+1, head]. W=0 disables window metrics.
type rollingWindow struct {
	cfg domain.Config
	w   map[string]*tokenWindow
}

func newRollingWindow(cfg domain.Config) *rollingWindow {
	return &rollingWindow{cfg: cfg, w: make(map[string]*tokenWindow)}
}

func (w *rollingWindow) token(token string) *tokenWindow {
	tw := w.w[token]
	if tw == nil {
		tw = &tokenWindow{}
		w.w[token] = tw
	}
	return tw
}

// push records the aggregate at height and immediately evicts the
// block that fell out of the trailing window.
func (w *rollingWindow) push(height uint64, token string, agg blockTransferAgg) {
	if w.cfg.ActiveWindowBlocks == 0 {
		return
	}
	tw := w.token(token)
	tw.heights = append(tw.heights, height)
	tw.entries = append(tw.entries, agg.clone())
	w.evict(token, height)
}

// pop removes the aggregate recorded exactly at height. It is used on
// block rollback; heights are contiguous so this is the last entry.
func (w *rollingWindow) pop(height uint64, token string) blockTransferAgg {
	tw := w.w[token]
	if tw == nil || len(tw.heights) == 0 {
		return newBlockTransferAgg()
	}
	last := len(tw.heights) - 1
	if tw.heights[last] != height {
		return newBlockTransferAgg()
	}
	agg := tw.entries[last]
	tw.heights = tw.heights[:last]
	tw.entries = tw.entries[:last]
	return agg
}

// evict drops retained entries below head-W+1.
func (w *rollingWindow) evict(token string, head uint64) {
	width := w.cfg.ActiveWindowBlocks
	tw := w.w[token]
	cut := 0
	if head >= width {
		floor := head - width + 1
		for cut < len(tw.heights) && tw.heights[cut] < floor {
			cut++
		}
	}
	if cut > 0 {
		tw.heights = append(tw.heights[:0], tw.heights[cut:]...)
		tw.entries = append(tw.entries[:0], tw.entries[cut:]...)
	}
}

// view aggregates the retained entries for a token.
func (w *rollingWindow) view(token string) windowView {
	view := windowView{volume: new(big.Int)}
	tw := w.w[token]
	if tw == nil {
		return view
	}
	active := make(map[string]struct{})
	for _, agg := range tw.entries {
		view.volume.Add(view.volume, agg.volume)
		view.count += agg.count
		for addr := range agg.addrs {
			active[addr] = struct{}{}
		}
	}
	view.address = uint64(len(active))
	return view
}

// reset clears all window state (used only on full rebuild start).
func (w *rollingWindow) reset() {
	for token := range w.w {
		delete(w.w, token)
	}
}
