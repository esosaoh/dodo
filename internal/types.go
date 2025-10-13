package internal

import "time"

type URLItem struct {
	URL       string
	Depth     int
	SourceURL string
	Timestamp time.Time
}

type Page struct {
	URL        string
	Title      string
	Content    string
	Links      []string
	StatusCode int
	CrawledAt  time.Time
}

type CrawlerConfig struct {
	MaxDepth       int
	MaxWorkers     int
	RateLimit      time.Duration
	UserAgent      string
	RequestTimeout time.Duration
}

func DefaultConfig() *CrawlerConfig {
	return &CrawlerConfig{
		MaxDepth:       3,
		MaxWorkers:     50,
		RateLimit:      1 * time.Second,
		UserAgent:      "MyWebCrawler/1.0",
		RequestTimeout: 10 * time.Second,
	}
}
