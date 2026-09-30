package chainmetrics

import "math/big"

// EventType categorizes a token balance event.
type EventType int

const (
	// Transfer is a regular value transfer between two addresses.
	Transfer EventType = iota
	// Mint is token issuance into an address.
	Mint
	// Burn is token destruction from an address.
	Burn
)

// Event is a single token balance change occurring inside a block.
type Event struct {
	TokenID string
	Type    EventType
	From    string
	To      string
	Amount  *big.Int
	Block   uint64
	// Index is the event's unique position inside its block.
	Index uint32
}

// Block is a set of events produced at one block height.
type Block struct {
	Number     uint64
	Hash       string
	ParentHash string
	Events     []Event
}

// Holding is one address and its current balance.
type Holding struct {
	Address string
	Balance *big.Int
}

// BalanceTier partitions holders by balance range [Min, Max).
type BalanceTier struct {
	Name string
	Min  *big.Int
	Max  *big.Int // nil means +infinity
}

// TokenMetrics is the metric set maintained for one token.
type TokenMetrics struct {
	TokenID string

	CirculatingSupply *big.Int
	HolderCount       int64
	TransferCount     int64

	// Windowed metrics.
	ActiveAddresses int64
	TransferVolume  *big.Int

	// TierCounts maps tier name to the number of holders inside it.
	TierCounts map[string]int64

	// TopHoldings are the largest holders, ordered descending.
	TopHoldings []Holding
}

// Snapshot is a complete, consistent metric view at one published block.
type Snapshot struct {
	BlockNumber uint64
	BlockHash   string
	Tokens      map[string]TokenMetrics
}

// MetricConfig configures metric definitions and window sizes.
type MetricConfig struct {
	// ActiveWindowBlocks is the rolling window size for active addresses.
	ActiveWindowBlocks uint64
	// VolumeWindowBlocks is the rolling window size for transfer volume.
	VolumeWindowBlocks uint64
	// TopN limits the reported largest holders.
	TopN int
	// Tiers defines balance buckets for holder distribution.
	Tiers []BalanceTier
}

// DefaultMetricConfig returns sensible default metric definitions.
func DefaultMetricConfig() MetricConfig {
	return normalizeMetricConfig(MetricConfig{})
}

// normalizeMetricConfig fills unset metric parameters with defaults and
// validates tier definitions.
func normalizeMetricConfig(cfg MetricConfig) MetricConfig {
	if cfg.ActiveWindowBlocks == 0 {
		cfg.ActiveWindowBlocks = 7
	}
	if cfg.VolumeWindowBlocks == 0 {
		cfg.VolumeWindowBlocks = 7
	}
	if cfg.TopN == 0 {
		cfg.TopN = 10
	}
	if len(cfg.Tiers) == 0 {
		cfg.Tiers = defaultTiers()
	}
	return cfg
}

// defaultTiers partitions holders into five balance bands.
func defaultTiers() []BalanceTier {
	bd := new(big.Int)
	return []BalanceTier{
		{Name: "dust", Min: bd.SetInt64(1), Max: big.NewInt(1_000)},
		{Name: "small", Min: big.NewInt(1_000), Max: big.NewInt(1_000_000)},
		{Name: "medium", Min: big.NewInt(1_000_000), Max: big.NewInt(1_000_000_000)},
		{Name: "large", Min: big.NewInt(1_000_000_000), Max: big.NewInt(1_000_000_000_000)},
		{Name: "whale", Min: big.NewInt(1_000_000_000_000), Max: nil},
	}
}

// Config configures the engine.
type Config struct {
	// DataDir is where WAL and snapshots are persisted. Empty disables persistence.
	DataDir string
	Metrics MetricConfig
	// HistoryBlocks bounds how many published block snapshots stay in memory.
	HistoryBlocks uint64
}
