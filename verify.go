package main

import (
	"bytes"
	"context"
	"net/url"
	"sort"
	"time"
)

type verifyItem struct {
	l     *link
	state *LinkState
	res   *checked
	ids   map[string]struct{}
	title string
}

// attachState consults the cache for one link: recently-verified healthy links
// short-circuit (checkOne skips the fetch), the rest carry prior state for
// conditional requests.
func (e *Engine) attachState(ctx context.Context, it *verifyItem, needsFragBody bool) {
	if e.Cache == nil {
		return
	}
	states, err := e.Cache.GetStates(ctx, []string{it.l.url})
	if err != nil {
		return
	}
	st := states[it.l.url]
	if st == nil {
		return
	}
	needsBody := e.cfg.CheckFragments && needsFragBody
	if st.Class == ClassAlive && !needsBody && time.Since(st.CheckedAt) < e.cfg.CacheTTL {
		it.res.verdict = Verdict{Class: ClassAlive, Reason: "cached", Confidence: 0.9}
		it.res.status = st.Status
		it.res.cached = true
		return
	}
	it.state = st
}

// assemble builds the report once crawling and checking have both finished.
func (e *Engine) assemble(cr *crawler, items []*verifyItem) ([]LinkResult, []*LinkState) {
	if e.cfg.Soft404 {
		e.retractSuspectSoft404s(items)
	}

	byURL := make(map[string]*verifyItem, len(items))
	for _, it := range items {
		byURL[it.l.url] = it
	}

	var results []LinkResult
	for _, l := range cr.reg.all() {
		switch {
		case l.malformed:
			r := e.finalize(l, &checked{verdict: Verdict{Class: ClassMalformed, Reason: "unparseable_href", Confidence: 0.95}}, nil)
			results = append(results, r)
			e.checkedOne(true)
		case cr.verified[l.url] != nil:
			r := e.finalize(l, cr.verified[l.url], cr.pageIDs[l.url])
			results = append(results, r)
			e.checkedOne(r.Broken())
		case byURL[l.url] != nil:
			it := byURL[l.url]
			results = append(results, e.finalize(it.l, it.res, it.ids))
		}
	}

	var states []*LinkState
	now := time.Now()
	for _, it := range items {
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

// retractSuspectSoft404s downgrades fingerprint matches on hosts where the
// fingerprint implicated most healthy responses (one-template hosts).
func (e *Engine) retractSuspectSoft404s(items []*verifyItem) {
	for _, it := range items {
		if it.res.verdict.Reason != "matches_404_fingerprint" {
			continue
		}
		fp := e.prints.peek(schemeOf(it.l.url), hostOf(it.l.url))
		if fp != nil && fp.suspect() {
			it.res.verdict = Verdict{Class: ClassAlive, Reason: "host_template_suspected", Confidence: 0.6}
		}
	}
}

// checkOne runs one fetch+classify attempt, reporting whether to retry.
func (e *Engine) checkOne(ctx context.Context, it *verifyItem) bool {
	if it.res.cached {
		return false
	}
	it.res.attempts++
	host := hostOf(it.l.url)

	if v, ok := e.health.trippedVerdict(host); ok {
		it.res.verdict = v
		return false
	}

	if err := e.sched.Acquire(ctx, host); err != nil {
		it.res.verdict = Verdict{Class: ClassUnknown, Reason: "cancelled", Confidence: 0}
		return false
	}
	// re-check after Acquire: the breaker may have tripped while we waited
	// out a Retry-After pause
	if v, ok := e.health.trippedVerdict(host); ok {
		e.sched.Release(host, FeedbackNeutral, 0)
		it.res.verdict = v
		return false
	}
	opts := FetchOpts{WantBody: true}
	if it.res.attempts > 1 {
		// a retry that needs the full timeout twice is almost never coming
		// back; spend half as long on it
		opts.Timeout = max(e.cfg.Timeout/2, 5*time.Second)
	}
	if it.state != nil {
		opts.ETag = it.state.ETag
		opts.LastModified = it.state.LastModified
	}
	start := time.Now()
	fr := e.fetcher.Fetch(ctx, it.l.url, opts)
	fb, ra := feedbackFor(fr)
	e.sched.Release(host, fb, ra)

	v := Classify(fr)
	e.trace(host, start, time.Since(start), fr.Status, v.Reason)
	e.health.record(host, fr.Err != nil, v)
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
			if fp.matches(fr.Body, it.l.url) {
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
	limit := e.cfg.MaxRetries
	// the expensive failures: timeouts burn wall time, 429 hosts make us wait
	// out their pause — one retry each, then report honestly
	if v.Reason == "timeout" || v.Reason == "rate_limited" {
		limit = min(limit, 1)
	}
	return v.Retryable && it.res.attempts <= limit
}

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
