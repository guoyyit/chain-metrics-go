package chainmetrics

import "math/big"

// windowEntry records one block's contribution to a rolling window.
type windowEntry struct {
	block  uint64
	active map[string]struct{}
	volume *big.Int
}

// rollingWindow maintains bounded per-block contributions for one token.
type rollingWindow struct {
	tokenID    string
	activeSize uint64
	volumeSize uint64

	entries []windowEntry
}

func newRollingWindow(tokenID string, activeSize, volumeSize uint64) *rollingWindow {
	return &rollingWindow{tokenID: tokenID, activeSize: activeSize, volumeSize: volumeSize}
}

// push records the contribution of a block in ascending order.
func (w *rollingWindow) push(block uint64, active []string, volume *big.Int) {
	entry := windowEntry{block: block, active: make(map[string]struct{}, len(active)), volume: new(big.Int)}
	for _, addr := range active {
		entry.active[addr] = struct{}{}
	}
	if volume != nil {
		entry.volume.Set(volume)
	}
	w.entries = append(w.entries, entry)
}

// truncate drops entries no longer needed by either window.
func (w *rollingWindow) truncate(block uint64) {
	size := w.activeSize
	if w.volumeSize > size {
		size = w.volumeSize
	}
	if size == 0 || block < size {
		return
	}
	cutoff := block - size + 1
	drop := 0
	for drop < len(w.entries) && w.entries[drop].block < cutoff {
		drop++
	}
	if drop > 0 {
		w.entries = append(w.entries[:0], w.entries[drop:]...)
	}
}

// activeCount returns the number of distinct active addresses in the window.
func (w *rollingWindow) activeCount() int64 {
	return int64(len(w.activeSet(w.activeSize)))
}

// activeSet returns distinct active addresses within activeSize blocks.
func (w *rollingWindow) activeSet(size uint64) map[string]struct{} {
	uniq := make(map[string]struct{})
	if len(w.entries) == 0 {
		return uniq
	}
	head := w.entries[len(w.entries)-1].block
	cutoff := uint64(1)
	if head >= size {
		cutoff = head - size + 1
	}
	for _, e := range w.entries {
		if e.block < cutoff {
			continue
		}
		for addr := range e.active {
			uniq[addr] = struct{}{}
		}
	}
	return uniq
}

// volumeSum returns total transfer volume in the window.
func (w *rollingWindow) volumeSum() *big.Int {
	sum := new(big.Int)
	if len(w.entries) == 0 {
		return sum
	}
	head := w.entries[len(w.entries)-1].block
	cutoff := uint64(1)
	if head >= w.volumeSize {
		cutoff = head - w.volumeSize + 1
	}
	for _, e := range w.entries {
		if e.block < cutoff {
			continue
		}
		sum.Add(sum, e.volume)
	}
	return sum
}

// clone returns a deep copy.
func (w *rollingWindow) clone() *rollingWindow {
	cp := &rollingWindow{
		tokenID:    w.tokenID,
		activeSize: w.activeSize,
		volumeSize: w.volumeSize,
		entries:    make([]windowEntry, len(w.entries)),
	}
	for i, e := range w.entries {
		ne := windowEntry{block: e.block, active: make(map[string]struct{}, len(e.active)), volume: new(big.Int).Set(e.volume)}
		for addr := range e.active {
			ne.active[addr] = struct{}{}
		}
		cp.entries[i] = ne
	}
	return cp
}
