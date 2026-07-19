package engine

import (
	"context"
	"time"

	"github.com/esosaoh/dodo/internal/cache"
	"github.com/esosaoh/dodo/internal/classify"
)

type Ref struct {
	Page     string `json:"page"`
	Fragment string `json:"fragment,omitempty"`
	Text     string `json:"text,omitempty"`
}

type LinkResult struct {
	URL              string         `json:"url"`
	Internal         bool           `json:"internal"`
	Class            classify.Class `json:"class"`
	Status           int            `json:"status,omitempty"`
	Reason           string         `json:"reason,omitempty"`
	Confidence       float64        `json:"confidence"`
	FinalURL         string         `json:"final_url,omitempty"`
	Attempts         int            `json:"attempts,omitempty"`
	Cached           bool           `json:"cached,omitempty"`
	MissingFragments []string       `json:"missing_fragments,omitempty"`
	Refs             []Ref          `json:"refs"`
	PrevClass classify.Class `json:"prev_class,omitempty"`
}

func (r *LinkResult) Broken() bool {
	return r.Class == classify.ClassDead || r.Class == classify.ClassSoft404 || r.Class == classify.ClassMalformed
}

type Report struct {
	Seed         string                 `json:"seed"`
	StartedAt    time.Time              `json:"started_at"`
	FinishedAt   time.Time              `json:"finished_at"`
	PagesCrawled int                    `json:"pages_crawled"`
	TotalLinks   int                    `json:"total_links"`
	Cached       int                    `json:"cached"`
	Counts       map[classify.Class]int `json:"counts"`
	Broken       int                    `json:"broken"`
	Results      []LinkResult           `json:"results"`
}

type Config struct {
	MaxDepth       int
	MaxPages       int
	Workers        int
	PerHostInit    int
	PerHostMax     int
	Timeout        time.Duration
	UserAgent      string
	CheckExternal  bool
	RespectRobots  bool
	Soft404        bool
	CheckFragments bool
	MaxRetries     int
	RetryBaseDelay time.Duration
	HostFailLimit  int // consecutive connect failures before writing a host off; 0 disables
	MaxBodyBytes   int64
	PageBodyBytes  int64
	CacheTTL       time.Duration
}

func DefaultConfig() *Config {
	return &Config{
		MaxDepth:       3,
		MaxPages:       2000,
		Workers:        256,
		PerHostInit:    8,
		PerHostMax:     16,
		Timeout:        10 * time.Second,
		UserAgent:      "dodo/0.1 (+https://github.com/esosaoh/dodo)",
		CheckExternal:  true,
		RespectRobots:  true,
		Soft404:        true,
		CheckFragments: true,
		MaxRetries:     2,
		RetryBaseDelay: time.Second,
		HostFailLimit:  5,
		MaxBodyBytes:   256 << 10,
		PageBodyBytes:  2 << 20,
		CacheTTL:       24 * time.Hour,
	}
}

type Phase string

const (
	PhaseCrawl  Phase = "crawl"
	PhaseVerify Phase = "verify"
	PhaseDone   Phase = "done"
)

type Progress struct {
	Phase        Phase `json:"phase"`
	PagesCrawled int   `json:"pages_crawled"`
	LinksFound   int   `json:"links_found"`
	LinksChecked int   `json:"links_checked"`
	LinksTotal   int   `json:"links_total"`
	Alive        int   `json:"alive"`
	Broken       int   `json:"broken"`
	Blocked      int   `json:"blocked"`
}

type ProgressFunc func(Progress)

// LinkCheckedFunc fires once per crawled page or verified link, so callers
// can stream results live instead of only seeing aggregate counts.
type LinkCheckedFunc func(url string, class classify.Class, status int, reason string)

type StateCache interface {
	GetStates(ctx context.Context, urls []string) (map[string]*cache.LinkState, error)
	PutStates(ctx context.Context, scan cache.ScanSummary, states []*cache.LinkState) error
}
