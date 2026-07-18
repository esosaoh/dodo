package engine

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"time"

	"github.com/esosaoh/dodo/internal/classify"
	"github.com/esosaoh/dodo/internal/fetch"
	"github.com/esosaoh/dodo/internal/scheduler"
)

type crawlItem struct {
	url   string
	depth int
}

type checked struct {
	verdict      classify.Verdict
	status       int
	finalURL     string
	redirected   bool
	attempts     int
	cached       bool
	etag         string
	lastModified string
	prevClass    classify.Class
}

type crawler struct {
	e          *Engine
	seed       string
	reg        *registry
	onDiscover func(l *link, firstRef Ref)

	mu            sync.Mutex
	internalHosts map[string]bool
	visited       map[string]bool
	verified      map[string]*checked
	pageIDs       map[string]map[string]struct{}
	pages         int
	pending       int
	closed        bool
	queue         chan crawlItem
	doneCh        chan struct{}
}

func newCrawler(e *Engine, seed string) *crawler {
	c := &crawler{
		e:             e,
		seed:          seed,
		reg:           newRegistry(),
		internalHosts: map[string]bool{hostOf(seed): true},
		visited:       make(map[string]bool),
		verified:      make(map[string]*checked),
		pageIDs:       make(map[string]map[string]struct{}),
		queue:         make(chan crawlItem, e.cfg.MaxPages+1),
		doneCh:        make(chan struct{}),
	}
	return c
}

func (c *crawler) run(ctx context.Context) {
	if !c.enqueue(c.seed, 0) {
		return
	}
	var wg sync.WaitGroup
	for i := 0; i < c.e.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range c.queue {
				c.process(ctx, item)
				c.finish()
			}
		}()
	}
	select {
	case <-ctx.Done():
	case <-c.doneCh:
	}
	c.closeQueue()
	wg.Wait()
}

func (c *crawler) enqueue(url string, depth int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.visited[url] || len(c.visited) >= c.e.cfg.MaxPages {
		return false
	}
	c.visited[url] = true
	c.pending++
	// buffer is sized to MaxPages (enforced via visited), so this never blocks
	c.queue <- crawlItem{url: url, depth: depth}
	return true
}

func (c *crawler) finish() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending--
	if c.pending == 0 && !c.closed {
		close(c.doneCh)
	}
}

func (c *crawler) closeQueue() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.queue)
	}
}

func (c *crawler) isInternal(host string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.internalHosts[host] {
		return true
	}
	return c.internalHosts[strings.TrimPrefix(host, "www.")] || c.internalHosts["www."+host]
}

func (c *crawler) process(ctx context.Context, item crawlItem) {
	host := hostOf(item.url)
	if c.e.cfg.RespectRobots && !c.e.robots.allowed(ctx, schemeOf(item.url), host, pathQueryOf(item.url)) {
		return // not crawled; if it's also a link target, verify still checks it
	}
	if err := c.e.sched.Acquire(ctx, host); err != nil {
		return
	}
	start := time.Now()
	fr := c.e.fetcher.Fetch(ctx, item.url, fetch.FetchOpts{WantBody: true, BodyCap: c.e.cfg.PageBodyBytes})
	fb, ra := scheduler.FeedbackFor(fr)
	c.e.sched.Release(host, fb, ra)

	verdict := classify.Classify(fr)
	c.e.trace(host, start, time.Since(start), fr.Status, "crawl")
	res := &checked{
		verdict:    verdict,
		status:     fr.Status,
		finalURL:   fr.FinalURL,
		redirected: fr.Redirected,
		attempts:   1,
		prevClass:  c.e.prevClassOf(ctx, item.url),
	}
	if fr.Err == nil {
		res.etag = fr.Header.Get("Etag")
		res.lastModified = fr.Header.Get("Last-Modified")
	}
	if c.e.OnLinkChecked != nil {
		c.e.OnLinkChecked(item.url, verdict.Class, fr.Status)
	}

	c.mu.Lock()
	c.verified[item.url] = res
	c.pages++
	pages := c.pages
	c.mu.Unlock()
	c.e.emitCrawl(pages, c.reg.size())

	if verdict.Class != classify.ClassAlive || fr.Body == nil || !fr.IsHTML() {
		return
	}

	finalHost := hostOf(fr.FinalURL)
	if item.depth == 0 && fr.Redirected {
		// seed redirected (e.g. example.com -> www.example.com); adopt the landing host
		c.mu.Lock()
		c.internalHosts[finalHost] = true
		c.mu.Unlock()
	}
	if !c.isInternal(finalHost) {
		return // redirected off-site; don't treat foreign content as our page
	}

	pd, err := ParsePage(bytes.NewReader(fr.Body), fr.FinalURL)
	if err != nil {
		return
	}

	c.mu.Lock()
	c.pageIDs[item.url] = pd.IDs
	c.mu.Unlock()

	for _, rl := range pd.Malformed {
		c.reg.addMalformed(rl.URL, Ref{Page: item.url, Text: rl.Text})
	}
	for _, rl := range pd.Links {
		internal := c.isInternal(hostOf(rl.URL))
		if !internal && !c.e.cfg.CheckExternal {
			continue
		}
		ref := Ref{Page: item.url, Fragment: rl.Fragment, Text: rl.Text}
		isNew, l := c.reg.add(rl.URL, internal, ref)
		crawlable := internal && !isNonHTMLURL(rl.URL)
		if crawlable && item.depth < c.e.cfg.MaxDepth {
			c.enqueue(rl.URL, item.depth+1)
		}
		// crawlable links are left for the post-crawl sweep (they may yet be
		// crawled); everything else starts verifying immediately
		if isNew && !crawlable && c.onDiscover != nil {
			l.submitted = true
			c.onDiscover(l, ref)
		}
	}
	c.e.emitCrawl(-1, c.reg.size())
}
