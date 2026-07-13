package store

import (
	"context"
	"errors"
	"time"

	"github.com/esosaoh/crawl/internal/engine"
)

var ErrNotFound = errors.New("not found")

type ScanStatus string

const (
	ScanRunning ScanStatus = "running"
	ScanDone    ScanStatus = "done"
	ScanFailed  ScanStatus = "failed"
)

type ScanRecord struct {
	ID        string         `bson:"_id" json:"id"`
	Seed      string         `bson:"seed" json:"seed"`
	Status    ScanStatus     `bson:"status" json:"status"`
	Error     string         `bson:"error,omitempty" json:"error,omitempty"`
	CreatedAt time.Time      `bson:"created_at" json:"created_at"`
	Report    *engine.Report `bson:"report,omitempty" json:"report,omitempty"`
}

// Store persists scan results and per-link check state. Implementations must
// also satisfy engine.StateCache so the engine can do incremental re-scans.
type Store interface {
	engine.StateCache

	CreateScan(ctx context.Context, rec *ScanRecord) error
	UpdateScan(ctx context.Context, rec *ScanRecord) error
	GetScan(ctx context.Context, id string) (*ScanRecord, error)
	ListScans(ctx context.Context, limit int) ([]*ScanRecord, error)
	Close(ctx context.Context) error
}
