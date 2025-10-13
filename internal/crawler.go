package internal

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Frontier struct {
	queue       chan URLItem
	visited     sync.Map
	maxDepth    int
	queueSize   int
	activeCount int
	mu          sync.Mutex
	doneCh      chan struct{}
	closed      bool
}

func NewFrontier(maxDepth int, bufferSize int) *Frontier {
	return &Frontier{
		queue:       make(chan URLItem, bufferSize),
		visited:     sync.Map{},
		maxDepth:    maxDepth,
		queueSize:   0,
		activeCount: 0,
		doneCh:      make(chan struct{}),
		closed:      false,
	}
}

func (f *Frontier) Add(item URLItem) bool {
	if item.Depth > f.maxDepth {
		return false
	}

	normalizedURL := normalizeForDedup(item.URL)

	if _, exists := f.visited.LoadOrStore(normalizedURL, true); exists {
		return false
	}

	select {
	case f.queue <- item:
		f.mu.Lock()
		f.queueSize++
		f.mu.Unlock()
		return true
	default:
		return false
	}
}

func (f *Frontier) Get() (URLItem, bool) {
	item, ok := <-f.queue
	if ok {
		f.mu.Lock()
		f.queueSize--
		f.activeCount++
		f.mu.Unlock()
	}
	return item, ok
}

func (f *Frontier) Done() {
	f.mu.Lock()
	f.activeCount--
	if f.queueSize == 0 && f.activeCount == 0 {
		select {
		case <-f.doneCh: // already closed
		default:
			close(f.doneCh)
		}
	}
	f.mu.Unlock()
}

func (f *Frontier) WaitUntilDone() {
	<-f.doneCh
}

func (f *Frontier) Has(url string) bool {
	normalized := normalizeForDedup(url)
	_, exists := f.visited.Load(normalized)
	return exists
}

func (f *Frontier) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.closed {
		close(f.queue)
		f.closed = true
	}
}

func (f *Frontier) Size() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queueSize
}

func normalizeForDedup(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}

	u.Fragment = ""
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)

	return u.String()
}

type RateLimiter struct {
	limiters map[string]*time.Ticker // Domain -> Ticker
	mu       sync.RWMutex
	delay    time.Duration
}

func NewRateLimiter(delay time.Duration) *RateLimiter {
	return &RateLimiter{
		limiters: make(map[string]*time.Ticker),
		delay:    delay,
	}
}

func (rl *RateLimiter) Wait(domain string) {
	rl.mu.Lock()
	ticker, exists := rl.limiters[domain]
	if !exists {
		ticker = time.NewTicker(rl.delay)
		rl.limiters[domain] = ticker
	}
	rl.mu.Unlock()

	<-ticker.C // block until tick
}

func GetDomain(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Host
}

type Fetcher struct {
	client    *http.Client
	userAgent string
	limiter   *RateLimiter
}

func NewFetcher(config *CrawlerConfig) *Fetcher {
	return &Fetcher{
		client: &http.Client{
			Timeout: config.RequestTimeout,
		},
		userAgent: config.UserAgent,
		limiter:   NewRateLimiter(config.RateLimit),
	}
}

func (f *Fetcher) Fetch(urlStr string) (*http.Response, error) {
	domain := GetDomain(urlStr)
	if domain != "" {
		f.limiter.Wait(domain) // rate limit per domain
	}

	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("User-Agent", f.userAgent)

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch failed: %w", err)
	}

	return resp, nil
}

type Crawler struct {
	config      *CrawlerConfig
	frontier    *Frontier
	fetcher     *Fetcher
	parser      *Parser
	wg          sync.WaitGroup
	stopCh      chan struct{}
	stopOnce    sync.Once
	stats       CrawlerStats
	statsMu     sync.Mutex
	seedDomains map[string]bool
}

type CrawlerStats struct {
	PagesProcessed int
	PagesFailed    int
	StartTime      time.Time
}

func NewCrawler(config *CrawlerConfig) *Crawler {
	return &Crawler{
		config:      config,
		frontier:    NewFrontier(config.MaxDepth, 50000),
		fetcher:     NewFetcher(config),
		parser:      NewParser(),
		stopCh:      make(chan struct{}),
		seedDomains: make(map[string]bool),
		stats: CrawlerStats{
			StartTime: time.Now(),
		},
	}
}

func (c *Crawler) Start(seedURLs []string) {
	fmt.Printf("\nStarting crawl with %d seed URLs\n", len(seedURLs))
	fmt.Printf("Max Depth: %d | Workers: %d | Rate Limit: %v\n",
		c.config.MaxDepth, c.config.MaxWorkers, c.config.RateLimit)
	fmt.Println()

	for _, seedURL := range seedURLs {
		domain := GetDomain(seedURL)
		if domain != "" {
			c.seedDomains[domain] = true
		}

		item := URLItem{
			URL:       seedURL,
			Depth:     0,
			SourceURL: "",
			Timestamp: time.Now(),
		}
		c.frontier.Add(item)
		fmt.Printf("Added seed: %s\n", seedURL)
	}
	fmt.Println()

	for i := 0; i < c.config.MaxWorkers; i++ {
		c.wg.Add(1)
		go c.worker(i)
	}

	fmt.Println("Waiting for all work to complete...")
	select {
	case <-c.frontier.doneCh:
		c.frontier.Close()
	case <-c.stopCh:
		// Stop() already closed the frontier
	}

	c.wg.Wait()
	c.PrintStats()
}

func (c *Crawler) worker(id int) {
	defer c.wg.Done()

	for {
		select {
		case <-c.stopCh:
			return
		default:
		}

		item, ok := c.frontier.Get()
		if !ok {
			return
		}

		select {
		case <-c.stopCh:
			c.frontier.Done()
			return
		default:
		}

		c.processURL(item)
		c.frontier.Done()
	}
}

func (c *Crawler) processURL(item URLItem) {
	select {
	case <-c.stopCh:
		return
	default:
	}

	fmt.Printf("[Depth %d] Crawling: %s\n", item.Depth, item.URL)

	resp, err := c.fetcher.Fetch(item.URL)
	if err != nil {
		select {
		case <-c.stopCh:
			return
		default:
		}

		fmt.Printf("  ✗ Failed to fetch: %v\n", err)
		c.incrementFailed()

		// Stop crawling if we hit timeout errors
		if strings.Contains(err.Error(), "context deadline exceeded") ||
			strings.Contains(err.Error(), "Client.Timeout exceeded") {
			c.Stop()
		}
		return
	}
	defer resp.Body.Close()

	select {
	case <-c.stopCh:
		return
	default:
	}

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("  ✗ Bad status: %d\n", resp.StatusCode)
		c.incrementFailed()
		return
	}

	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(strings.ToLower(contentType), "text/html") {
		fmt.Printf("Skipping non-HTML content: %s\n", contentType)
		return
	}

	page, err := c.parser.ParseHTML(resp.Body, item.URL)
	if err != nil {
		select {
		case <-c.stopCh:
			return
		default:
		}

		fmt.Printf("  ✗ Parse error: %v\n", err)
		c.incrementFailed()

		if strings.Contains(err.Error(), "context deadline exceeded") ||
			strings.Contains(err.Error(), "Client.Timeout") {
			c.Stop()
		}
		return
	}

	c.incrementProcessed()
	fmt.Printf("  ✓ Success! Found %d links\n", len(page.Links))

	linksAdded := 0
	linksSkipped := 0
	for _, link := range page.Links {
		newItem := URLItem{
			URL:       link,
			Depth:     item.Depth + 1,
			SourceURL: item.URL,
			Timestamp: time.Now(),
		}
		if c.frontier.Add(newItem) {
			linksAdded++
		}
	}
	if linksAdded > 0 {
		fmt.Printf("  → Added %d new URLs to queue", linksAdded)
		if linksSkipped > 0 {
			fmt.Printf(" (skipped %d off-domain)", linksSkipped)
		}
		fmt.Println()
	}

	// TODO: Save page to MongoDB
}

func (c *Crawler) getTotalProcessed() int {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	return c.stats.PagesProcessed
}

func (c *Crawler) incrementProcessed() {
	c.statsMu.Lock()
	c.stats.PagesProcessed++
	c.statsMu.Unlock()
}

func (c *Crawler) incrementFailed() {
	c.statsMu.Lock()
	c.stats.PagesFailed++
	c.statsMu.Unlock()
}

func (c *Crawler) PrintStats() {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()

	elapsed := time.Since(c.stats.StartTime)
	pagesPerHour := float64(c.stats.PagesProcessed) / elapsed.Hours()

	fmt.Println("\n" + strings.Repeat("=", 50))
	fmt.Println("\n-------------------CRAWL STATS-------------------")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Printf("Pages processed: %d\n", c.stats.PagesProcessed)
	fmt.Printf("Pages failed:    %d\n", c.stats.PagesFailed)
	fmt.Printf("Time elapsed:    %v\n", elapsed.Round(time.Second))
	fmt.Printf("Pages/hour:      %.0f\n", pagesPerHour)
	fmt.Println(strings.Repeat("=", 50))
}

func (c *Crawler) Stop() {
	c.stopOnce.Do(func() {
		fmt.Println("\nStopping crawler...")
		close(c.stopCh)
		c.frontier.Close()
	})
}
