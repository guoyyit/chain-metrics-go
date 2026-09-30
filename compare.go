package chainmetrics

import "math/big"

// snapshotsEqual reports whether two snapshots hold identical metrics.
func snapshotsEqual(a, b Snapshot) bool {
	if a.BlockNumber != b.BlockNumber || a.BlockHash != b.BlockHash {
		return false
	}
	if len(a.Tokens) != len(b.Tokens) {
		return false
	}
	for tokenID, ma := range a.Tokens {
		mb, ok := b.Tokens[tokenID]
		if !ok || !metricsEqual(ma, mb) {
			return false
		}
	}
	return true
}

func metricsEqual(a, b TokenMetrics) bool {
	if a.TokenID != b.TokenID ||
		a.HolderCount != b.HolderCount ||
		a.TransferCount != b.TransferCount ||
		a.ActiveAddresses != b.ActiveAddresses {
		return false
	}
	if bigCmp(a.CirculatingSupply, b.CirculatingSupply) != 0 {
		return false
	}
	if bigCmp(a.TransferVolume, b.TransferVolume) != 0 {
		return false
	}
	if len(a.TierCounts) != len(b.TierCounts) {
		return false
	}
	for k, v := range a.TierCounts {
		if b.TierCounts[k] != v {
			return false
		}
	}
	if len(a.TopHoldings) != len(b.TopHoldings) {
		return false
	}
	for i := range a.TopHoldings {
		if a.TopHoldings[i].Address != b.TopHoldings[i].Address ||
			bigCmp(a.TopHoldings[i].Balance, b.TopHoldings[i].Balance) != 0 {
			return false
		}
	}
	return true
}

func bigCmp(a, b *big.Int) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	default:
		return a.Cmp(b)
	}
}
