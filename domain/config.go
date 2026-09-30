package domain

import (
	"errors"
	"math/big"
)

// Config controls metric definitions and rolling-window parameters.
type Config struct {
	// ActiveWindowBlocks is the size (in blocks) of the rolling window
	// covering recent transfer volume and active addresses. The current
	// block is included. Zero disables window metrics.
	ActiveWindowBlocks uint64

	// TopHolderCount is the size of the top-holder ranking used to
	// report concentration. Zero disables the ranking.
	TopHolderCount int

	// BalanceTierBounds (ascending, inclusive lower bounds in token
	// base units) defines balance histogram tiers. An address with
	// balance b belongs to the first tier whose bound v satisfies b >= v.
	// Tiers[0] therefore counts non-zero balances when bounds[0] == 1.
	BalanceTierBounds []*big.Int

	// HistoryRetainBlocks bounds in-memory and on-disk snapshot
	// history. Snapshots older than head minus this value are pruned;
	// queries below that height return ErrHeightPruned.
	HistoryRetainBlocks uint64

	// CheckpointInterval is the number of published blocks between
	// durable state checkpoints used to accelerate crash recovery.
	CheckpointInterval uint64

	// DataDir is the on-disk state directory.
	DataDir string
}

// DefaultConfig returns sane defaults: a 64-block rolling window,
// a top-10 holder ranking, five balance tiers, 10k blocks of history
// and a checkpoint every 256 blocks.
func DefaultConfig() Config {
	return Config{
		ActiveWindowBlocks: 64,
		TopHolderCount:     10,
		BalanceTierBounds: []*big.Int{
			big.NewInt(1),
			big.NewInt(1_000),
			big.NewInt(100_000),
			big.NewInt(10_000_000),
			big.NewInt(1_000_000_000),
		},
		HistoryRetainBlocks: 10_000,
		CheckpointInterval:  256,
		DataDir:             "./metrics-data",
	}
}

// Validate checks configuration invariants and normalizes zero values.
func (c *Config) Validate() error {
	for i := 1; i < len(c.BalanceTierBounds); i++ {
		if c.BalanceTierBounds[i].Cmp(c.BalanceTierBounds[i-1]) <= 0 {
			return errors.New("BalanceTierBounds must be strictly ascending")
		}
	}
	if c.DataDir == "" {
		c.DataDir = "./metrics-data"
	}
	return nil
}
