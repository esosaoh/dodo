package engine

import (
	"bytes"
	"context"
	"math/rand"
	"net/url"
	"sort"
	"sync"
	"time"
)

type verifyItem struct {
	l     *link
	state *LinkState
	res   *checked
	ids   map[string]struct{}
	title string
}

// verifyPhase checks every registered link that the crawl didn't already
// verify, consulting the state cache first and retrying transient failures in
// backoff rounds at the end rather than inline.
func (e *Engine) verifyPhase(ctx context.Context, links []*link, cr *crawler) ([]LinkResult, []*LinkState) {
	e.setTotal(len(links))
	var results []LinkResult

	var toCheck []*verifyItem
	for _, l := range links {
		if res, ok := cr.verified[l.url]; ok {
			r := e.finalize(l, res, cr.pageIDs[l.url])
			results = append(results, r)
			e.checkedOne(r.Broken())
			continue
		}
		toCheck = append(toCheck, &verifyItem{l: l, res: &checked{}})
	}

	toCheck = e.applyCache(ctx, toCheck, &results)

	pending := e.runPass(ctx, toCheck)
	for round := 1; round <= e.cfg.MaxRetries && len(pending) > 0; round++ {
		e.setPhase(PhaseRetry)
		delay := e.cfg.RetryBaseDelay * (1 << (round - 1))
		delay += time.Duration(rand.Int63n(int64(delay/4) + 1))
		select {
		case <-ctx.Done():
		case <-time.After(delay):
		}
		if ctx.Err() != nil {
			break
		}
		pending = e.runPass(ctx, pending)
	}
	for range pending { // only non-empty if the scan was cancelled mid-retry
		e.checkedOne(false)
	}

	var states []*LinkState
	now := time.Now()
	for _, it := range toCheck {
		results = append(results, e.finalize(it.l, it.res, it.ids))
		if it.res.cached || it.res.attempts == 0 {
			continue
		}
		st := &LinkState{
			URL:          it.l.url,
			Class:        it.res.verdict.Class,
			Status:       it.res.status,
			ETag:         it.res.etag,
			LastModified: it.res.lastModified,
			CheckedAt:    now,
		}
		if prev := it.state; prev != nil {
			st.Fails, st.Successes = prev.Fails, prev.Successes
			if st.ETag == "" {
				st.ETag = prev.ETag
			}
			if st.LastModified == "" {
				st.LastModified = prev.LastModified
			}
		}
		if st.Class == ClassAlive {
			st.Successes++
		} else {
			st.Fails++
		}
		states = append(states, st)
	}
	return results, states
}

// applyCache short-circuits links that were verified healthy recently and
// attaches prior state (for conditional requests) to the rest.
func (e *Engine) applyCache(ctx context.Context, items []*verifyItem, results *[]LinkResult) []*verifyItem {
	if e.Cache == nil || len(items) == 0 {
		return items
	}
	urls := make([]string, len(items))
	for i, it := range items {
		urls[i] = it.l.url
	}
	states, err := e.Cache.GetStates(ctx, urls)
	if err != nil {
		return items
	}
	kept := items[:0]
	for _, it := range items {
		st := states[it.l.url]
		if st == nil {
			kept = append(kept, it)
			continue
		}
		needsBody := e.cfg.CheckFragments && len(it.l.fragments()) > 0
		if st.Class == ClassAlive && !needsBody && time.Since(st.CheckedAt) < e.cfg.CacheTTL {
			it.res.verdict = Verdict{Class: ClassAlive, Reason: "cached", Confidence: 0.9}
			it.res.status = st.Status
			it.res.cached = true
			*results = append(*results, e.finalize(it.l, it.res, nil))
			e.checkedOne(false)
			continue
		}
		it.state = st
		kept = append(kept, it)
	}
	return kept
}

// runPass checks items concurrently and returns the ones that still warrant a
// retry round.
func (e *Engine) runPass(ctx context.Context, items []*verifyItem) []*verifyItem {
	if len(items) == 0 {
		return nil
	}
	ch := make(chan *verifyItem)
	var mu sync.Mutex
	var retries []*verifyItem
	var wg sync.WaitGroup

	workers := min(e.cfg.Workers, len(items))
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range ch {
				if e.checkOne(ctx, it) {
					mu.Lock()
					retries = append(retries, it)
					mu.Unlock()
				} else {
					e.checkedOne(it.res.verdict.Class == ClassDead || it.res.verdict.Class == ClassSoft404)
				}
			}
		}()
	}
	for _, it := range items {
		ch <- it
	}
	close(ch)
	wg.Wait()
	return retries
}

// checkOne fetches and classifies a single link once. It reports whether the
// verdict is transient and another attempt is allowed.
func (e *Engine) checkOne(ctx context.Context, it *verifyItem) bool {
	it.res.attempts++
	host := hostOf(it.l.url)

	if err := e.sched.Acquire(ctx, host); err != nil {
		it.res.verdict = Verdict{Class: ClassUnknown, Reason: "cancelled", Confidence: 0}
		return false
	}
	opts := FetchOpts{WantBody: true}
	if it.state != nil {
		opts.ETag = it.state.ETag
		opts.LastModified = it.state.LastModified
	}
	fr := e.fetcher.Fetch(ctx, it.l.url, opts)
	fb, ra := feedbackFor(fr)
	e.sched.Release(host, fb, ra)

	v := Classify(fr)
	it.res.status = fr.Status
	it.res.finalURL = fr.FinalURL
	it.res.redirected = fr.Redirected
	if fr.Err == nil {
		it.res.etag = fr.Header.Get("Etag")
		it.res.lastModified = fr.Header.Get("Last-Modified")
	}

	if v.Class == ClassAlive && fr.Status == 200 && fr.IsHTML() && len(fr.Body) > 0 {
		if pd, err := ParsePage(bytes.NewReader(fr.Body), fr.FinalURL); err == nil {
			it.ids = pd.IDs
			it.title = pd.Title
		}
		if e.cfg.Soft404 {
			fp := e.prints.forHost(ctx, schemeOf(it.l.url), host)
			if fp.matches(fr.Body) {
				v = Verdict{Class: ClassSoft404, Reason: "matches_404_fingerprint", Confidence: 0.85}
			} else if titleLooks404(it.title) {
				v = Verdict{Class: ClassSoft404, Reason: "title_indicates_404", Confidence: 0.6}
			}
		}
	}

	if it.res.attempts > 1 && v.Class == ClassAlive {
		v.Reason = "flaky"
		v.Confidence = 0.8
	}
	it.res.verdict = v
	return v.Retryable && it.res.attempts <= e.cfg.MaxRetries
}

// finalize turns a checked outcome into the reported result, including
// fragment validation against the target page's element IDs.
func (e *Engine) finalize(l *link, res *checked, ids map[string]struct{}) LinkResult {
	v := FinalizeVerdict(res.verdict, res.attempts)
	r := LinkResult{
		URL:        l.url,
		Internal:   l.internal,
		Class:      v.Class,
		Status:     res.status,
		Reason:     v.Reason,
		Confidence: v.Confidence,
		Attempts:   res.attempts,
		Cached:     res.cached,
		Refs:       l.refs,
	}
	if res.redirected {
		r.FinalURL = res.finalURL
	}
	if e.cfg.CheckFragments && v.Class == ClassAlive && ids != nil {
		var missing []string
		for _, frag := range l.fragments() {
			if frag == "top" { // browsers scroll to top for #top with no element
				continue
			}
			if _, ok := ids[frag]; !ok {
				missing = append(missing, frag)
			}
		}
		sort.Strings(missing)
		r.MissingFragments = missing
	}
	return r
}

func schemeOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" {
		return "https"
	}
	return u.Scheme
}
