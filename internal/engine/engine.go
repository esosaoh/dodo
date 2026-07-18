package engine

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/esosaoh/dodo/internal/cache"
	"github.com/esosaoh/dodo/internal/classify"
	"github.com/esosaoh/dodo/internal/fetch"
	"github.com/esosaoh/dodo/internal/health"
	"github.com/esosaoh/dodo/internal/scheduler"
)

// Engine runs one scan; single-use.
type Engine struct {
	cfg     *Config
	fetcher *fetch.Fetcher
	sched   *scheduler.Scheduler
	prints  *fingerprints
	health  *health.HostHealth
	robots  *robotsCache

	// optional; set before Run
	Cache      StateCache
	OnProgress ProgressFunc

	progMu sync.Mutex
	prog   Progress

	traceMu    sync.Mutex
	traceFile  *os.File
	traceStart time.Time
}

func NewEngine(cfg *Config) *Engine {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	e := &Engine{
		cfg:     cfg,
		fetcher: fetch.NewFetcher(cfg.Timeout, cfg.UserAgent, cfg.MaxBodyBytes),
		sched:   scheduler.NewScheduler(cfg.PerHostInit, cfg.PerHostMax),
		health:  health.NewHostHealth(cfg.HostFailLimit),
	}
	e.prints = newFingerprints(e)
	e.robots = newRobotsCache(e)
	if path := os.Getenv("DODO_TRACE"); path != "" {
		if f, err := os.Create(path); err == nil {
			e.traceFile = f
			e.traceStart = time.Now()
		}
	}
	return e
}

// trace appends one CSV row per fetch when DODO_TRACE is set: start-offset ms,
// duration ms, host, status, reason.
func (e *Engine) trace(host string, start time.Time, dur time.Duration, status int, reason string) {
	if e.traceFile == nil {
		return
	}
	e.traceMu.Lock()
	fmt.Fprintf(e.traceFile, "%d,%d,%s,%d,%s\n",
		start.Sub(e.traceStart).Milliseconds(), dur.Milliseconds(), host, status, reason)
	e.traceMu.Unlock()
}

func (e *Engine) Run(ctx context.Context, seed string) (*Report, error) {
	seedURL, err := NormalizeURL(seed)
	if err != nil {
		return nil, err
	}
	// Wake any workers blocked on host gates when the scan is cancelled.
	stopWatch := context.AfterFunc(ctx, e.sched.Shutdown)
	defer stopWatch()

	rep := &Report{
		Seed:      seedURL,
		StartedAt: time.Now(),
		Counts:    make(map[classify.Class]int),
	}

	e.setPhase(PhaseCrawl)
	pool := e.newCheckPool(ctx, e.cfg.Workers)
	cr := newCrawler(e, seedURL)
	cr.onDiscover = func(l *link, firstRef Ref) {
		it := &verifyItem{l: l, res: &checked{}}
		e.attachState(ctx, it, firstRef.Fragment != "")
		pool.add(it)
	}
	cr.run(ctx)

	links := cr.reg.all()
	e.setPhase(PhaseVerify)
	e.setTotal(len(links))
	for _, l := range links {
		if l.malformed || l.submitted {
			continue
		}
		if _, ok := cr.verified[l.url]; ok {
			continue
		}
		l.submitted = true
		it := &verifyItem{l: l, res: &checked{}}
		e.attachState(ctx, it, len(l.fragments()) > 0)
		pool.add(it)
	}
	pool.finishAdds()
	items := pool.wait()
	results, states := e.assemble(cr, items)

	sort.Slice(results, func(i, j int) bool {
		si, sj := classify.Severity(results[i].Class), classify.Severity(results[j].Class)
		if si != sj {
			return si < sj
		}
		if results[i].Confidence != results[j].Confidence {
			return results[i].Confidence > results[j].Confidence
		}
		return results[i].URL < results[j].URL
	})

	for i := range results {
		rep.Counts[results[i].Class]++
		if results[i].Broken() {
			rep.Broken++
		}
		if results[i].Cached {
			rep.Cached++
		}
	}
	rep.Results = results
	rep.PagesCrawled = cr.pages
	rep.TotalLinks = len(links)
	rep.FinishedAt = time.Now()
	e.setPhase(PhaseDone)

	if e.Cache != nil && len(states) > 0 {
		summary := cache.ScanSummary{
			Seed: rep.Seed, StartedAt: rep.StartedAt, FinishedAt: rep.FinishedAt,
			PagesCrawled: rep.PagesCrawled, TotalLinks: rep.TotalLinks, Broken: rep.Broken,
		}
		e.Cache.PutStates(context.WithoutCancel(ctx), summary, states)
	}
	return rep, nil
}

func (e *Engine) snapshotLocked() Progress {
	return e.prog
}

func (e *Engine) emitLocked() {
	if e.OnProgress != nil {
		e.OnProgress(e.snapshotLocked())
	}
}

func (e *Engine) setPhase(p Phase) {
	e.progMu.Lock()
	e.prog.Phase = p
	e.emitLocked()
	e.progMu.Unlock()
}

func (e *Engine) setTotal(n int) {
	e.progMu.Lock()
	e.prog.LinksTotal = n
	e.emitLocked()
	e.progMu.Unlock()
}

func (e *Engine) checkedOne(broken bool) {
	e.progMu.Lock()
	e.prog.LinksChecked++
	if broken {
		e.prog.Broken++
	}
	e.emitLocked()
	e.progMu.Unlock()
}

func (e *Engine) emitCrawl(pages, found int) {
	e.progMu.Lock()
	if pages >= 0 {
		e.prog.PagesCrawled = pages
	}
	e.prog.LinksFound = found
	e.prog.LinksTotal = found
	e.emitLocked()
	e.progMu.Unlock()
}
