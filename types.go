package main

import (
	"context"
	"time"
)

type Class string

const (
	ClassAlive   Class = "alive"
	ClassDead    Class = "dead"
	ClassSoft404 Class = "soft_404"
	ClassBlocked Class = "blocked"
	ClassUnknown Class = "unknown"
)

func severity(c Class) int {
	switch c {
	case ClassDead:
		return 0
	case ClassSoft404:
		return 1
	case ClassBlocked:
		return 2
	case ClassUnknown:
		return 3
	default:
		return 4
	}
}

type Ref struct {
	Page     string `json:"page"`
	Fragment string `json:"fragment,omitempty"`
	Text     string `json:"text,omitempty"`
}

type LinkResult struct {
	URL              string   `json:"url"`
	Internal         bool     `json:"internal"`
	Class            Class    `json:"class"`
	Status           int      `json:"status,omitempty"`
	Reason           string   `json:"reason,omitempty"`
	Confidence       float64  `json:"confidence"`
	FinalURL         string   `json:"final_url,omitempty"`
	Attempts         int      `json:"attempts,omitempty"`
	Cached           bool     `json:"cached,omitempty"`
	MissingFragments []string `json:"missing_fragments,omitempty"`
	Refs             []Ref    `json:"refs"`
}

func (r *LinkResult) Broken() bool {
	return r.Class == ClassDead || r.Class == ClassSoft404
}

type Report struct {
	Seed         string        `json:"seed"`
	StartedAt    time.Time     `json:"started_at"`
	FinishedAt   time.Time     `json:"finished_at"`
	PagesCrawled int           `json:"pages_crawled"`
	TotalLinks   int           `json:"total_links"`
	Cached       int           `json:"cached"`
	Counts       map[Class]int `json:"counts"`
	Broken       int           `json:"broken"`
	Results      []LinkResult  `json:"results"`
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
		Workers:        128,
		PerHostInit:    4,
		PerHostMax:     16,
		Timeout:        15 * time.Second,
		UserAgent:      "dodo/0.1 (+https://github.com/esosaoh/dodo)",
		CheckExternal:  true,
		Soft404:        true,
		CheckFragments: true,
		MaxRetries:     2,
		RetryBaseDelay: 2 * time.Second,
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
	PhaseRetry  Phase = "retry"
	PhaseDone   Phase = "done"
)

type Progress struct {
	Phase        Phase `json:"phase"`
	PagesCrawled int   `json:"pages_crawled"`
	LinksFound   int   `json:"links_found"`
	LinksChecked int   `json:"links_checked"`
	LinksTotal   int   `json:"links_total"`
	Broken       int   `json:"broken"`
}

type ProgressFunc func(Progress)

type LinkState struct {
	URL          string
	Class        Class
	Status       int
	ETag         string
	LastModified string
	CheckedAt    time.Time
	Fails        int
	Successes    int
}

type StateCache interface {
	GetStates(ctx context.Context, urls []string) (map[string]*LinkState, error)
	PutStates(ctx context.Context, states []*LinkState) error
}
