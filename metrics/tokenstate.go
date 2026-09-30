package metrics

import (
	"math/big"

	"github.com/guoyyit/chain-metrics-go/domain"
)

// tokenState holds the incrementally maintained ledger and metrics for
// one token. Every mutation in apply has an exact inverse in rollback,
// so forks can be undone block by block.
type tokenState struct {
	token string

	circulatingSupply *big.Int
	totalMinted       *big.Int
	totalBurned       *big.Int

	transferCount       uint64
	totalTransferVolume *big.Int

	// balances stores only non-zero balances; zero entries are deleted,
	// keeping memory proportional to live holders rather than history.
	balances map[string]*big.Int

	// tierCounts[i] is the number of holders in balance tier i.
	tierCounts []uint64

	// sumSquares is the exact sum of balance^2 over all holders,
	// enabling O(1) Herfindahl maintenance per balance change.
	sumSquares *big.Int

	holders *holderIndex

	cfg domain.Config
}

func newTokenState(token string, cfg domain.Config) *tokenState {
	return &tokenState{
		token:               token,
		circulatingSupply:   new(big.Int),
		totalMinted:         new(big.Int),
		totalBurned:         new(big.Int),
		totalTransferVolume: new(big.Int),
		balances:            make(map[string]*big.Int),
		tierCounts:          make([]uint64, len(cfg.BalanceTierBounds)),
		sumSquares:          new(big.Int),
		holders:             newHolderIndex(),
		cfg:                 cfg,
	}
}

// tierOf returns the tier index for a strictly positive balance, or -1
// for a zero balance. Tiers use inclusive ascending lower bounds.
func (t *tokenState) tierOf(balance *big.Int) int {
	if balance == nil || balance.Sign() <= 0 {
		return -1
	}
	idx := -1
	for i, bound := range t.cfg.BalanceTierBounds {
		if balance.Cmp(bound) >= 0 {
			idx = i
		} else {
			break
		}
	}
	return idx
}

// updateBalance applies a signed delta to an address and adjusts
// tiers, sum-of-squares and the holder ranking.
func (t *tokenState) updateBalance(addr string, delta *big.Int) {
	if delta.Sign() == 0 || addr == domain.ZeroAddress {
		return
	}
	old := t.balances[addr]
	oldVal := new(big.Int)
	if old != nil {
		oldVal.Set(old)
	}
	oldTier := t.tierOf(oldVal)

	newVal := new(big.Int).Add(oldVal, delta)
	newTier := t.tierOf(newVal)

	if oldTier >= 0 {
		t.tierCounts[oldTier]--
	}
	if newTier >= 0 {
		t.tierCounts[newTier]++
	}

	// sumSquares += new^2 - old^2
	sq := new(big.Int).Mul(newVal, newVal)
	sq.Sub(sq, new(big.Int).Mul(oldVal, oldVal))
	t.sumSquares.Add(t.sumSquares, sq)

	if newVal.Sign() == 0 {
		delete(t.balances, addr)
	} else {
		t.balances[addr] = newVal
	}
	t.holders.observe(addr, newVal)
}

// apply mutates the state for one event and returns its contribution
// to the rolling window (transfers only).
func (t *tokenState) apply(ev domain.Event) blockTransferAgg {
	agg := blockTransferAgg{
		volume: new(big.Int),
		addrs:  make(map[string]struct{}),
	}
	amount := ev.Amount
	if amount == nil {
		amount = new(big.Int)
	}

	switch ev.Type {
	case domain.EventMint:
		t.totalMinted.Add(t.totalMinted, amount)
		t.circulatingSupply.Add(t.circulatingSupply, amount)
		t.updateBalance(ev.To, amount)
	case domain.EventBurn:
		t.totalBurned.Add(t.totalBurned, amount)
		t.circulatingSupply.Sub(t.circulatingSupply, amount)
		t.updateBalance(ev.From, new(big.Int).Neg(amount))
	default:
		t.transferCount++
		t.totalTransferVolume.Add(t.totalTransferVolume, amount)
		agg.volume.Set(amount)
		agg.count = 1
		if ev.From != domain.ZeroAddress {
			agg.addrs[ev.From] = struct{}{}
		}
		if ev.To != domain.ZeroAddress {
			agg.addrs[ev.To] = struct{}{}
		}
		t.updateBalance(ev.From, new(big.Int).Neg(amount))
		t.updateBalance(ev.To, amount)
	}
	return agg
}

// rollback reverses exactly one previously applied event. The window
// aggregate returned at apply time is passed back by the caller.
func (t *tokenState) rollback(ev domain.Event, agg blockTransferAgg) {
	amount := ev.Amount
	if amount == nil {
		amount = new(big.Int)
	}
	switch ev.Type {
	case domain.EventMint:
		t.totalMinted.Sub(t.totalMinted, amount)
		t.circulatingSupply.Sub(t.circulatingSupply, amount)
		t.updateBalance(ev.To, new(big.Int).Neg(amount))
	case domain.EventBurn:
		t.totalBurned.Sub(t.totalBurned, amount)
		t.circulatingSupply.Add(t.circulatingSupply, amount)
		t.updateBalance(ev.From, amount)
	default:
		_ = agg
		t.transferCount--
		t.totalTransferVolume.Sub(t.totalTransferVolume, amount)
		t.updateBalance(ev.To, new(big.Int).Neg(amount))
		t.updateBalance(ev.From, amount)
	}
}

// concentration computes the top-K share (sum of top K balances over
// circulating supply) and the Herfindahl-Hirschman index
// (sum of share^2 over every holder) using big.Float at fixed
// precision. Both are exact fractions evaluated at 53-bit precision.
func (t *tokenState) concentration() (top []domain.HolderBalance, topShare, hhi float64) {
	k := t.cfg.TopHolderCount
	entries := t.holders.top(k)
	topSum := new(big.Int)
	top = make([]domain.HolderBalance, 0, len(entries))
	for _, e := range entries {
		topSum.Add(topSum, e.balance)
		top = append(top, domain.HolderBalance{
			Address: e.addr,
			Balance: new(big.Int).Set(e.balance),
		})
	}
	if t.circulatingSupply.Sign() > 0 {
		topShare = ratio(topSum, t.circulatingSupply)
		hhi = ratio(t.sumSquares, new(big.Int).Mul(t.circulatingSupply, t.circulatingSupply))
	}
	return top, topShare, hhi
}

// ratio returns numerator/denominator as float64; caller guarantees
// denominator is positive.
func ratio(num, den *big.Int) float64 {
	const prec = 53
	nf := new(big.Float).SetPrec(prec).SetInt(num)
	df := new(big.Float).SetPrec(prec).SetInt(den)
	q, _ := new(big.Float).Quo(nf, df).Float64()
	return q
}

// snapshot builds the non-window snapshot fields for this token.
func (t *tokenState) snapshot(height uint64, hash string, ts int64, view windowView) *domain.TokenSnapshot {
	tiers := make([]uint64, len(t.tierCounts))
	copy(tiers, t.tierCounts)

	top, topShare, hhi := t.concentration()
	topCopy := make([]domain.HolderBalance, len(top))
	copy(topCopy, top)

	return &domain.TokenSnapshot{
		Token:     t.token,
		Height:    height,
		BlockHash: hash,
		Timestamp: ts,

		CirculatingSupply: new(big.Int).Set(t.circulatingSupply),
		TotalMinted:       new(big.Int).Set(t.totalMinted),
		TotalBurned:       new(big.Int).Set(t.totalBurned),

		TransferCount:        t.transferCount,
		TotalTransferVolume:  new(big.Int).Set(t.totalTransferVolume),
		WindowTransferVolume: new(big.Int).Set(view.volume),
		WindowTransferCount:  view.count,
		ActiveAddresses:      view.address,

		BalanceTiers: tiers,

		TopHolders:      topCopy,
		TopHolderShare:  topShare,
		HerfindahlIndex: hhi,
	}
}
