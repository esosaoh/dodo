package engine

import (
	"context"
	"sync"
	"time"
)

type Feedback int

const (
	FeedbackNeutral Feedback = iota
	FeedbackSuccess
	FeedbackBackoff
)

// responses slower than this don't grow the host's window
const slowLatency = 2 * time.Second

// Scheduler is per-host AIMD congestion control: fast clean responses grow a
// host's concurrency window, 429/503/timeouts halve it.
type Scheduler struct {
	mu    sync.Mutex
	gates map[string]*hostGate

	initial float64
	min     float64
	max     float64
}

type hostGate struct {
	mu         sync.Mutex
	cond       *sync.Cond
	limit      float64
	inflight   int
	pauseUntil time.Time
	lastDrop   time.Time
	closed     bool
}

func NewScheduler(cfg *Config) *Scheduler {
	return &Scheduler{
		gates:   make(map[string]*hostGate),
		initial: float64(cfg.PerHostInit),
		min:     1,
		max:     float64(cfg.PerHostMax),
	}
}

func (s *Scheduler) gate(host string) *hostGate {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.gates[host]
	if !ok {
		g = &hostGate{limit: s.initial}
		g.cond = sync.NewCond(&g.mu)
		s.gates[host] = g
	}
	return g
}

func (s *Scheduler) Acquire(ctx context.Context, host string) error {
	g := s.gate(host)
	g.mu.Lock()
	defer g.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if g.closed {
			return context.Canceled
		}
		now := time.Now()
		if now.Before(g.pauseUntil) {
			wait := g.pauseUntil.Sub(now)
			g.mu.Unlock()
			select {
			case <-ctx.Done():
				g.mu.Lock()
				return ctx.Err()
			case <-time.After(wait):
			}
			g.mu.Lock()
			continue
		}
		if g.inflight < int(g.limit) {
			g.inflight++
			return nil
		}
		g.cond.Wait()
	}
}

func (s *Scheduler) Release(host string, fb Feedback, retryAfter time.Duration) {
	g := s.gate(host)
	g.mu.Lock()
	g.inflight--
	switch fb {
	case FeedbackSuccess:
		g.limit = min(g.limit+1.0/g.limit, s.max)
	case FeedbackBackoff:
		now := time.Now()
		// one decrease per window, or a burst of failures collapses the limit
		if now.Sub(g.lastDrop) > 2*time.Second {
			g.limit = max(s.min, g.limit/2)
			g.lastDrop = now
		}
		if retryAfter > 0 {
			// an explicit Retry-After means stop, not "slightly less"
			g.limit = s.min
			// honor it only up to a point: camping on a host that asked for
			// a minute stalls the whole scan tail
			pause := min(retryAfter, 15*time.Second)
			until := now.Add(pause)
			if until.After(g.pauseUntil) {
				g.pauseUntil = until
			}
		}
	}
	g.cond.Broadcast()
	g.mu.Unlock()
}

func (s *Scheduler) Limit(host string) float64 {
	g := s.gate(host)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.limit
}

// Shutdown wakes blocked Acquire calls so workers can observe cancellation.
func (s *Scheduler) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.gates {
		g.mu.Lock()
		g.closed = true
		g.pauseUntil = time.Time{}
		g.cond.Broadcast()
		g.mu.Unlock()
	}
}

func feedbackFor(r *FetchResult) (Feedback, time.Duration) {
	if r.Err != nil {
		v := classifyErr(r.Err)
		if v.Reason == "timeout" {
			return FeedbackBackoff, 0
		}
		return FeedbackNeutral, 0
	}
	if r.Status == 429 || r.Status == 503 {
		return FeedbackBackoff, r.RetryAfter
	}
	// only 2xx/3xx grow the window: fast 4xx often means a WAF is refusing us,
	// and speeding up at a host that's blocking us is the wrong response
	if r.Status < 400 && r.Latency < slowLatency {
		return FeedbackSuccess, 0
	}
	return FeedbackNeutral, 0
}
