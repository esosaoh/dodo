package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/esosaoh/dodo/internal/cache"
	"github.com/esosaoh/dodo/internal/classify"
)

func html(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, body)
}

const softPage = `<html><head><title>Hmm</title></head><body>
Sorry, we couldn't find the page you were looking for anywhere on this site.
Try searching for what you need or head back to the homepage instead.
</body></html>`

func newExtServer(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var flakyHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			html(w, `<html><head><title>External home</title></head><body>
Welcome to the external site. We publish articles about databases, distributed
systems, cooking, travel, photography and a great many other unrelated topics.
</body></html>`)
		case "/ok":
			html(w, `<html><head><title>A real article</title></head><body>
This is a real article with genuine content about interesting subjects that is
clearly different from an error page in every possible way.
</body></html>`)
		case "/flaky":
			if atomic.AddInt32(&flakyHits, 1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			html(w, `<html><head><title>Flaky page</title></head><body>
Recovered now. This page works on the second attempt and has real content.
</body></html>`)
		case "/gone":
			http.NotFound(w, r)
		default:
			// Soft 404: HTTP 200 with an error page, for any unknown path
			// including the engine's fingerprint probe.
			html(w, softPage)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &flakyHits
}

func newSeedServer(t *testing.T, extURL string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		html(w, fmt.Sprintf(`<html><head><title>Seed home</title></head><body>
<a href="/a">page a</a>
<a href="/b">page b</a>
<a href="/missing">broken internal</a>
<a href="%[1]s/ok">good external</a>
<a href="%[1]s/gone">dead external</a>
<a href="%[1]s/soft-thing">soft dead external</a>
<a href="%[1]s/flaky">flaky external</a>
</body></html>`, extURL))
	})
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) {
		html(w, `<html><head><title>Page A</title></head><body>
<h2 id="real">A real section</h2>
<a href="/b#nowhere">b with missing anchor</a>
</body></html>`)
	})
	mux.HandleFunc("/b", func(w http.ResponseWriter, r *http.Request) {
		html(w, `<html><head><title>Page B</title></head><body>
<a href="/a#real">a good anchor</a>
<a href="/a#fake">a missing anchor</a>
</body></html>`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func testConfig() *Config {
	cfg := DefaultConfig()
	cfg.Workers = 8
	cfg.MaxDepth = 3
	cfg.MaxPages = 50
	cfg.MaxRetries = 1
	cfg.RetryBaseDelay = 20 * time.Millisecond
	cfg.Timeout = 5 * time.Second
	return cfg
}

func findResult(t *testing.T, rep *Report, url string) *LinkResult {
	t.Helper()
	for i := range rep.Results {
		if rep.Results[i].URL == url {
			return &rep.Results[i]
		}
	}
	t.Fatalf("no result for %s in report (have %d results)", url, len(rep.Results))
	return nil
}

func TestScanEndToEnd(t *testing.T) {
	ext, flakyHits := newExtServer(t)
	seed := newSeedServer(t, ext.URL)

	e := NewEngine(testConfig())
	rep, err := e.Run(context.Background(), seed.URL)
	if err != nil {
		t.Fatal(err)
	}

	if rep.PagesCrawled != 4 { // /, /a, /b, /missing
		t.Errorf("pages crawled = %d, want 4", rep.PagesCrawled)
	}

	if r := findResult(t, rep, seed.URL+"/missing"); r.Class != classify.ClassDead || r.Status != 404 {
		t.Errorf("/missing: got class=%s status=%d, want dead/404", r.Class, r.Status)
	}
	if r := findResult(t, rep, ext.URL+"/gone"); r.Class != classify.ClassDead {
		t.Errorf("ext /gone: got class=%s, want dead", r.Class)
	}
	if r := findResult(t, rep, ext.URL+"/ok"); r.Class != classify.ClassAlive {
		t.Errorf("ext /ok: got class=%s (%s), want alive", r.Class, r.Reason)
	}

	if r := findResult(t, rep, ext.URL+"/soft-thing"); r.Class != classify.ClassSoft404 {
		t.Errorf("ext /soft-thing: got class=%s (%s), want soft_404", r.Class, r.Reason)
	} else if r.Reason != "matches_404_fingerprint" {
		t.Errorf("ext /soft-thing: reason=%s, want matches_404_fingerprint", r.Reason)
	}

	if r := findResult(t, rep, ext.URL+"/flaky"); r.Class != classify.ClassAlive || r.Attempts != 2 {
		t.Errorf("ext /flaky: got class=%s attempts=%d, want alive after 2 attempts", r.Class, r.Attempts)
	}
	if got := atomic.LoadInt32(flakyHits); got != 2 {
		t.Errorf("flaky endpoint hit %d times, want 2", got)
	}

	if r := findResult(t, rep, seed.URL+"/b"); !slices.Contains(r.MissingFragments, "nowhere") {
		t.Errorf("/b: missing fragments = %v, want to include 'nowhere'", r.MissingFragments)
	}
	rA := findResult(t, rep, seed.URL+"/a")
	if !slices.Contains(rA.MissingFragments, "fake") {
		t.Errorf("/a: missing fragments = %v, want to include 'fake'", rA.MissingFragments)
	}
	if slices.Contains(rA.MissingFragments, "real") {
		t.Errorf("/a: fragment 'real' exists but was reported missing")
	}

	if rep.Broken < 3 {
		t.Errorf("broken = %d, want at least 3 (missing, gone, soft-thing)", rep.Broken)
	}
	// Results must be sorted problems-first.
	if len(rep.Results) > 0 && rep.Results[0].Class == classify.ClassAlive && rep.Broken > 0 {
		t.Error("results are not sorted with problems first")
	}
}

// memCache is a trivial in-memory StateCache for testing incremental scans.
type memCache struct {
	states map[string]*cache.LinkState
	puts   int
}

func (m *memCache) GetStates(_ context.Context, urls []string) (map[string]*cache.LinkState, error) {
	out := make(map[string]*cache.LinkState)
	for _, u := range urls {
		if s, ok := m.states[u]; ok {
			out[u] = s
		}
	}
	return out, nil
}

func (m *memCache) PutStates(_ context.Context, states []*cache.LinkState) error {
	m.puts++
	for _, s := range states {
		m.states[s.URL] = s
	}
	return nil
}

func TestIncrementalRescanSkipsFreshLinks(t *testing.T) {
	ext, _ := newExtServer(t)
	seed := newSeedServer(t, ext.URL)
	cache := &memCache{states: make(map[string]*cache.LinkState)}

	cfg := testConfig()
	cfg.CheckFragments = false // fragment checks force re-fetching

	e1 := NewEngine(cfg)
	e1.Cache = cache
	rep1, err := e1.Run(context.Background(), seed.URL)
	if err != nil {
		t.Fatal(err)
	}
	if rep1.Cached != 0 {
		t.Errorf("first scan reported %d cached links, want 0", rep1.Cached)
	}
	if len(cache.states) == 0 {
		t.Fatal("first scan persisted no link states")
	}

	e2 := NewEngine(cfg)
	e2.Cache = cache
	rep2, err := e2.Run(context.Background(), seed.URL)
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Cached == 0 {
		t.Error("second scan used no cached results")
	}
	// Dead links must always be re-checked, never served from cache.
	if r := findResult(t, rep2, ext.URL+"/gone"); r.Cached {
		t.Error("dead link was served from cache")
	}
}

func TestTimeoutLinksRetryOnlyOnce(t *testing.T) {
	var hits int32
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(2 * time.Second)
	}))
	t.Cleanup(slow.Close)

	seed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		html(w, fmt.Sprintf(`<html><body><a href="%s/slow">slow</a></body></html>`, slow.URL))
	}))
	t.Cleanup(seed.Close)

	cfg := testConfig()
	cfg.Timeout = 300 * time.Millisecond
	cfg.MaxRetries = 2
	cfg.Soft404 = false

	rep, err := NewEngine(cfg).Run(context.Background(), seed.URL)
	if err != nil {
		t.Fatal(err)
	}
	r := findResult(t, rep, slow.URL+"/slow")
	if r.Class != classify.ClassUnknown || r.Reason != "timeout" {
		t.Errorf("got %s/%s, want unknown/timeout", r.Class, r.Reason)
	}
	if r.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 (one retry for timeouts, not MaxRetries)", r.Attempts)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("slow endpoint hit %d times, want 2", got)
	}
}
