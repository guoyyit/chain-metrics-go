// Package service is the public facade of the local on-chain token
// metrics computation and query service.
package service

import (
	"github.com/guoyyit/chain-metrics-go/domain"
	"github.com/guoyyit/chain-metrics-go/engine"
	"github.com/guoyyit/chain-metrics-go/rebuild"
	"github.com/guoyyit/chain-metrics-go/store"
)

// MetricsService wires the durable store and the ingestion/query
// engine together and owns the lifecycle.
type MetricsService struct {
	cfg domain.Config
	st  *store.FileStore
	eng *engine.Engine
}

// Open opens (or creates) a service rooted at cfg.DataDir and performs
// crash recovery before returning. On return the engine is at the last
// published block and ready for concurrent ingestion and queries.
func Open(cfg domain.Config) (*MetricsService, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	eng := engine.New(cfg, st)
	if err := eng.Recover(); err != nil {
		st.Close()
		return nil, err
	}
	return &MetricsService{cfg: cfg, st: st, eng: eng}, nil
}

// Close releases the durable layer.
func (s *MetricsService) Close() error { return s.st.Close() }

// IngestBlock ingests one possibly duplicate or out-of-order block.
func (s *MetricsService) IngestBlock(b *domain.Block) error {
	return s.eng.IngestBlock(b)
}

// QuerySnapshot returns the consistent snapshot at height (0=latest).
func (s *MetricsService) QuerySnapshot(height uint64) (*domain.BlockSnapshot, error) {
	return s.eng.SnapshotAtHeight(height)
}

// QueryToken returns one token's metrics at height (0=latest).
func (s *MetricsService) QueryToken(height uint64, token string) (*domain.TokenSnapshot, error) {
	return s.eng.TokenAtHeight(height, token)
}

// Head returns the current published head.
func (s *MetricsService) Head() domain.HeadPointer { return s.eng.Head() }

// VerifyIncremental runs the full-replay reference path at the
// current tip and compares it with the incremental snapshot. It does
// not touch or rebuild the live state.
func (s *MetricsService) VerifyIncremental() error {
	tip := s.eng.Head()
	chain, err := rebuild.CanonicalChain(s.st, tip.Hash)
	if err != nil {
		return err
	}
	got, err := s.eng.SnapshotAtHeight(tip.Height)
	if err != nil {
		return err
	}
	return rebuild.Verify(s.cfg, chain, got)
}
