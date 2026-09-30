package domain

import "errors"

var (
	// ErrHeightNotYetPublished indicates a query above the current head.
	ErrHeightNotYetPublished = errors.New("height not yet published")
	// ErrHeightPruned indicates a query below the retained history floor.
	ErrHeightPruned = errors.New("height pruned")
	// ErrNotFound is returned when a token or block is unknown.
	ErrNotFound = errors.New("not found")
	// ErrInvalidBlock is returned for structurally invalid input.
	ErrInvalidBlock = errors.New("invalid block")
)
