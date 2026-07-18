package engine

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"github.com/esosaoh/dodo/internal/classify"
)

// checkPool runs link checks with per-item retry timers: a retryable item
// re-queues itself after its own backoff instead of the whole scan pausing
// between retry rounds. Items can be added while checks are running.
type checkPool struct {
	e   *Engine
	ctx context.Context

	mu      sync.Mutex
	cond    *sync.Cond
	queue   []*verifyItem
	pending int
	noMore  bool
	items   []*verifyItem

	done      chan struct{}
	doneOnce  sync.Once
	wg        sync.WaitGroup
	stopWatch func() bool
}

func (e *Engine) newCheckPool(ctx context.Context, workers int) *checkPool {
	p := &checkPool{e: e, ctx: ctx, done: make(chan struct{})}
	p.cond = sync.NewCond(&p.mu)
	// wake blocked workers if the scan is cancelled
	p.stopWatch = context.AfterFunc(ctx, func() {
		p.mu.Lock()
		p.cond.Broadcast()
		p.mu.Unlock()
	})
	for i := 0; i < workers; i++ {
		p.wg.Add(1)
		go p.worker()
	}
	return p
}

func (p *checkPool) add(it *verifyItem) {
	p.mu.Lock()
	p.pending++
	p.items = append(p.items, it)
	p.queue = append(p.queue, it)
	p.cond.Signal()
	p.mu.Unlock()
}

func (p *checkPool) finishAdds() {
	p.mu.Lock()
	p.noMore = true
	if p.pending == 0 {
		p.close()
	}
	p.mu.Unlock()
}

// wait blocks until every item reached a final verdict (or the scan was
// cancelled) and returns all items ever added.
func (p *checkPool) wait() []*verifyItem {
	defer p.stopWatch()
	select {
	case <-p.done:
	case <-p.ctx.Done():
	}
	p.mu.Lock()
	p.cond.Broadcast()
	items := p.items
	p.mu.Unlock()
	p.wg.Wait()
	return items
}

func (p *checkPool) worker() {
	defer p.wg.Done()
	for {
		p.mu.Lock()
		for len(p.queue) == 0 {
			if p.closed() || p.ctx.Err() != nil {
				p.mu.Unlock()
				return
			}
			p.cond.Wait()
		}
		it := p.queue[0]
		p.queue = p.queue[1:]
		p.mu.Unlock()

		if p.e.checkOne(p.ctx, it) && p.ctx.Err() == nil {
			delay := p.e.cfg.RetryBaseDelay * (1 << (it.res.attempts - 1))
			if delay > 0 {
				delay += time.Duration(rand.Int63n(int64(delay/4) + 1))
			}
			time.AfterFunc(delay, func() { p.requeue(it) })
			continue
		}

		class := it.res.verdict.Class
		p.e.checkedOne(class, class == classify.ClassDead || class == classify.ClassSoft404)
		p.mu.Lock()
		p.pending--
		if p.pending == 0 && p.noMore {
			p.close()
		}
		p.mu.Unlock()
	}
}

func (p *checkPool) requeue(it *verifyItem) {
	p.mu.Lock()
	p.queue = append(p.queue, it)
	p.cond.Signal()
	p.mu.Unlock()
}

// close must be called with p.mu held.
func (p *checkPool) close() {
	p.doneOnce.Do(func() {
		close(p.done)
		p.cond.Broadcast()
	})
}

func (p *checkPool) closed() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}
