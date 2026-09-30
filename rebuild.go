package chainmetrics

import "math/big"

// RebuildFromScratch recomputes every metric from genesis using full
// recomputation. It is the reference path used to verify incremental results.
func RebuildFromScratch(blocks []Block, cfg MetricConfig) (map[uint64]Snapshot, error) {
	cfg = normalizeMetricConfig(cfg)
	states := make(map[string]*tokenState)
	wins := make(map[string]*rollingWindow)
	out := make(map[uint64]Snapshot)

	for _, b := range blocks {
		snap := Snapshot{
			BlockNumber: b.Number,
			BlockHash:   b.Hash,
			Tokens:      make(map[string]TokenMetrics),
		}
		events := b.Events
		contrib := make(map[string]*blockContribution)
		for _, ev := range events {
			st, ok := states[ev.TokenID]
			if !ok {
				st = newTokenState(ev.TokenID, cfg)
				states[ev.TokenID] = st
				wins[ev.TokenID] = newRollingWindow(ev.TokenID,
					cfg.ActiveWindowBlocks, cfg.VolumeWindowBlocks)
			}
			st.apply(ev)
			c := contrib[ev.TokenID]
			if c == nil {
				c = newBlockContribution()
				contrib[ev.TokenID] = c
			}
			if ev.From != zeroAddress {
				c.active = append(c.active, ev.From)
			}
			if ev.To != zeroAddress {
				c.active = append(c.active, ev.To)
			}
			if ev.Type == Transfer {
				c.volume.Add(c.volume, ev.Amount)
			}
		}
		for tokenID, st := range states {
			win := wins[tokenID]
			if c := contrib[tokenID]; c != nil {
				win.push(b.Number, c.active, c.volume)
			}
			win.truncate(b.Number)
			snap.Tokens[tokenID] = st.snapshot(win.activeCount(), win.volumeSum())
		}
		out[b.Number] = snap
	}
	return out, nil
}

type blockContribution struct {
	active []string
	volume *big.Int
}

func newBlockContribution() *blockContribution {
	return &blockContribution{volume: new(big.Int)}
}

// VerifyAgainstRebuild compares the engine's incremental snapshots with a full
// rebuild and reports the first diverging height, or 0 when identical.
func (e *Engine) VerifyAgainstRebuild(blocks []Block) (firstMismatch uint64, ok bool, err error) {
	reference, err := RebuildFromScratch(blocks, e.cfg.Metrics)
	if err != nil {
		return 0, false, err
	}
	for height := range reference {
		got, exists := e.QueryAt(height)
		if !exists {
			// A pruned height is not queryable; skip rather than fail.
			if height < e.lowestHeight() {
				continue
			}
			return height, false, nil
		}
		if !snapshotsEqual(got, reference[height]) {
			return height, false, nil
		}
	}
	return 0, true, nil
}

func (e *Engine) lowestHeight() uint64 {
	var lowest uint64
	first := true
	for h := range e.snapshots {
		if first || h < lowest {
			lowest, first = h, false
		}
	}
	return lowest
}
