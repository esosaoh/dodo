package main

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Engine runs one scan; single-use.
type Engine struct {
	cfg     *Config
	fetcher *Fetcher
	sched   *Scheduler
	prints  *fingerprints
	health  *hostHealth
	robots  *robotsCache

	// optional; set before Run
	Cache      StateCache
	OnProgress ProgressFunc

	progMu sync.Mutex
	prog   Progress
}

func newEngine(cfg *Config) *Engine {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	e := &Engine{
		cfg:     cfg,
		fetcher: NewFetcher(cfg),
		sched:   NewScheduler(cfg),
		health:  newHostHealth(cfg.HostFailLimit),
	}
	e.prints = newFingerprints(e)
	e.robots = newRobotsCache(e)
	return e
}

func (e *Engine) Run(ctx context.Context, seed string) (*Report, error) {
	seedURL, err := normalizeRawURL(seed)
	if err != nil {
		return nil, err
	}
	// Wake any workers blocked on host gates when the scan is cancelled.
	stopWatch := context.AfterFunc(ctx, e.sched.Shutdown)
	defer stopWatch()

	rep := &Report{
		Seed:      seedURL,
		StartedAt: time.Now(),
		Counts:    make(map[Class]int),
	}

	e.setPhase(PhaseCrawl)
	cr := newCrawler(e, seedURL)
	cr.run(ctx)

	links := cr.reg.all()
	e.setPhase(PhaseVerify)
	results, states := e.verifyPhase(ctx, links, cr)

	if e.Cache != nil && len(states) > 0 {
		e.Cache.PutStates(context.WithoutCancel(ctx), states)
	}

	sort.Slice(results, func(i, j int) bool {
		si, sj := severity(results[i].Class), severity(results[j].Class)
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
	e.emitLocked()
	e.progMu.Unlock()
}
