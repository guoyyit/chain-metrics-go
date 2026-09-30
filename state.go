package chainmetrics

import (
	"math/big"
	"sort"
)

// zeroAddress marks the null address used for mint/burn counter parties.
const zeroAddress = ""

var zero = big.NewInt(0)

// tokenState is the mutable metric state of one token.
type tokenState struct {
	tokenID string

	balances map[string]*big.Int

	supply      *big.Int
	holderCount int64
	transferCnt int64

	tierCounts map[string]int64
	topN       int
	tiers      []BalanceTier
}

func newTokenState(tokenID string, cfg MetricConfig) *tokenState {
	s := &tokenState{
		tokenID:    tokenID,
		balances:   make(map[string]*big.Int),
		supply:     new(big.Int),
		tierCounts: make(map[string]int64),
		topN:       cfg.TopN,
		tiers:      cfg.Tiers,
	}
	for _, tier := range cfg.Tiers {
		s.tierCounts[tier.Name] = 0
	}
	return s
}

// balance returns an address balance (nil-safe, never nil).
func (s *tokenState) balance(addr string) *big.Int {
	if b, ok := s.balances[addr]; ok {
		return b
	}
	return zero
}

// setBalance updates one balance, maintaining holder count and tier counts.
func (s *tokenState) setBalance(addr string, next *big.Int) {
	old := s.balance(addr)
	if old.Sign() > 0 {
		s.holderCount--
		s.tierCounts[s.tierName(old)]--
	}
	if next.Sign() > 0 {
		s.holderCount++
		s.tierCounts[s.tierName(next)]++
		cp := new(big.Int).Set(next)
		s.balances[addr] = cp
	} else {
		delete(s.balances, addr)
	}
}

// tierName returns the name of the tier containing bal (assumed positive).
func (s *tokenState) tierName(bal *big.Int) string {
	for _, tier := range s.tiers {
		if bal.Cmp(tier.Min) >= 0 && (tier.Max == nil || bal.Cmp(tier.Max) < 0) {
			return tier.Name
		}
	}
	return ""
}

// move transfers delta from one address to another. The zero address is the
// null party used by mint (empty From) and burn (empty To).
func (s *tokenState) move(from, to string, delta *big.Int) {
	if delta.Sign() <= 0 {
		return
	}
	if from != zeroAddress {
		next := new(big.Int).Sub(s.balance(from), delta)
		if next.Sign() < 0 {
			// Balances must never go negative: clamp malformed/overdrawn
			// events to the available funds rather than corrupting state.
			next.SetInt64(0)
		}
		s.setBalance(from, next)
	}
	if to != zeroAddress {
		s.setBalance(to, new(big.Int).Add(s.balance(to), delta))
	}
}

// apply applies one event to the state.
func (s *tokenState) apply(e Event) {
	switch e.Type {
	case Mint:
		s.move(zeroAddress, e.To, e.Amount)
		s.supply.Add(s.supply, e.Amount)
	case Burn:
		s.move(e.From, zeroAddress, e.Amount)
		s.supply.Sub(s.supply, e.Amount)
	case Transfer:
		s.move(e.From, e.To, e.Amount)
	}
	s.transferCnt++
}

// revert undoes one event previously applied.
func (s *tokenState) revert(e Event) {
	switch e.Type {
	case Mint:
		s.supply.Sub(s.supply, e.Amount)
		s.move(e.To, zeroAddress, e.Amount)
	case Burn:
		s.supply.Add(s.supply, e.Amount)
		s.move(zeroAddress, e.From, e.Amount)
	case Transfer:
		s.move(e.To, e.From, e.Amount)
	}
	s.transferCnt--
}

// snapshot produces an immutable TokenMetrics copy, with window values and
// top-N holdings filled in by the caller.
func (s *tokenState) snapshot(active int64, volume *big.Int) TokenMetrics {
	m := TokenMetrics{
		TokenID:           s.tokenID,
		CirculatingSupply: new(big.Int).Set(s.supply),
		HolderCount:       s.holderCount,
		TransferCount:     s.transferCnt,
		ActiveAddresses:   active,
		TransferVolume:    new(big.Int).Set(volume),
		TierCounts:        make(map[string]int64, len(s.tierCounts)),
	}
	for name, cnt := range s.tierCounts {
		m.TierCounts[name] = cnt
	}
	m.TopHoldings = s.topHoldings()
	return m
}

// topHoldings returns the largest holders, descending by balance.
func (s *tokenState) topHoldings() []Holding {
	if s.topN <= 0 || len(s.balances) == 0 {
		return nil
	}
	out := make([]Holding, 0, len(s.balances))
	for addr, bal := range s.balances {
		out = append(out, Holding{Address: addr, Balance: new(big.Int).Set(bal)})
	}
	sort.Slice(out, func(i, j int) bool {
		if c := out[i].Balance.Cmp(out[j].Balance); c != 0 {
			return c > 0
		}
		return out[i].Address < out[j].Address
	})
	if len(out) > s.topN {
		out = out[:s.topN]
	}
	return out
}

// clone returns a deep copy of the state.
func (s *tokenState) clone() *tokenState {
	cp := &tokenState{
		tokenID:     s.tokenID,
		balances:    make(map[string]*big.Int, len(s.balances)),
		supply:      new(big.Int).Set(s.supply),
		holderCount: s.holderCount,
		transferCnt: s.transferCnt,
		tierCounts:  make(map[string]int64, len(s.tierCounts)),
		topN:        s.topN,
		tiers:       s.tiers,
	}
	for addr, bal := range s.balances {
		cp.balances[addr] = new(big.Int).Set(bal)
	}
	for name, cnt := range s.tierCounts {
		cp.tierCounts[name] = cnt
	}
	return cp
}
