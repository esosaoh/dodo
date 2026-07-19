package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
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

func TestSoft404NotAssertedLiveBeforeRetraction(t *testing.T) {
	ext, _ := newExtServer(t)
	seed := newSeedServer(t, ext.URL)

	var streamed sync.Map
	e := NewEngine(testConfig())
	e.OnLinkChecked = func(url string, class classify.Class, status int, reason string) {
		streamed.Store(url, class)
	}

	rep, err := e.Run(context.Background(), seed.URL)
	if err != nil {
		t.Fatal(err)
	}

	r := findResult(t, rep, ext.URL+"/soft-thing")
	if r.Class != classify.ClassSoft404 || r.Reason != "matches_404_fingerprint" {
		t.Fatalf("precondition: expected soft-thing to end up soft_404/matches_404_fingerprint in the final report, got %s/%s", r.Class, r.Reason)
	}

	v, ok := streamed.Load(ext.URL + "/soft-thing")
	if !ok {
		t.Fatal("expected soft-thing to have streamed live")
	}
	if v.(classify.Class) != classify.ClassAlive {
		t.Errorf("streamed class = %s, want alive (a fingerprint match isn't trustworthy until retraction runs at the end of the scan)", v)
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

func (m *memCache) PutStates(_ context.Context, _ cache.ScanSummary, states []*cache.LinkState) error {
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

func TestPrevClassTracksAcrossScans(t *testing.T) {
	var extHits, subpageHits int32
	ext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&extHits, 1) == 1 {
			html(w, `<html><body>external page, alive on the first scan</body></html>`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ext.Close)

	mux := http.NewServeMux()
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		html(w, fmt.Sprintf(`<html><body>
<a href="/subpage">subpage</a>
<a href="%s">external</a>
</body></html>`, ext.URL))
	})
	mux.HandleFunc("/subpage", func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&subpageHits, 1) == 1 {
			html(w, `<html><body>internal subpage, alive on the first scan</body></html>`)
			return
		}
		http.NotFound(w, r)
	})
	seed := httptest.NewServer(mux)
	t.Cleanup(seed.Close)

	cache := &memCache{states: make(map[string]*cache.LinkState)}
	cfg := testConfig()
	cfg.Soft404 = false
	cfg.CheckFragments = false
	cfg.CacheTTL = 0 // force re-verification on the second scan instead of trusting the cache

	e1 := NewEngine(cfg)
	e1.Cache = cache
	rep1, err := e1.Run(context.Background(), seed.URL)
	if err != nil {
		t.Fatal(err)
	}
	if r := findResult(t, rep1, seed.URL+"/subpage"); r.Class != classify.ClassAlive || r.PrevClass != "" {
		t.Errorf("first scan /subpage: class=%s prevClass=%q, want alive with no prior record", r.Class, r.PrevClass)
	}
	if r := findResult(t, rep1, ext.URL+"/"); r.Class != classify.ClassAlive || r.PrevClass != "" {
		t.Errorf("first scan external: class=%s prevClass=%q, want alive with no prior record", r.Class, r.PrevClass)
	}

	e2 := NewEngine(cfg)
	e2.Cache = cache
	rep2, err := e2.Run(context.Background(), seed.URL)
	if err != nil {
		t.Fatal(err)
	}
	if r := findResult(t, rep2, seed.URL+"/subpage"); r.Class != classify.ClassDead || r.PrevClass != classify.ClassAlive {
		t.Errorf("second scan /subpage: class=%s prevClass=%s, want dead with prevClass alive", r.Class, r.PrevClass)
	}
	if r := findResult(t, rep2, ext.URL+"/"); r.Class != classify.ClassDead || r.PrevClass != classify.ClassAlive {
		t.Errorf("second scan external: class=%s prevClass=%s, want dead with prevClass alive", r.Class, r.PrevClass)
	}
}

func TestSoft404PrevClassSurvivesRealCache(t *testing.T) {
	var toggle int32
	ext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			html(w, `<html><head><title>Home</title></head><body>
Welcome to the real external site with genuine homepage content about many
different topics, products and services offered here.
</body></html>`)
		case r.URL.Path == "/soft-thing" && atomic.LoadInt32(&toggle) == 1:
			html(w, `<html><head><title>Real Article</title></head><body>
This is a real article with genuinely distinct content about an interesting
and specific subject matter, unrelated to anything else on this site.
</body></html>`)
		default:
			html(w, softPage)
		}
	}))
	t.Cleanup(ext.Close)

	seed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		html(w, fmt.Sprintf(`<html><body><a href="%s/soft-thing">thing</a></body></html>`, ext.URL))
	}))
	t.Cleanup(seed.Close)

	store, err := cache.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := testConfig()
	cfg.CacheTTL = 0

	e1 := NewEngine(cfg)
	e1.Cache = store
	rep1, err := e1.Run(context.Background(), seed.URL)
	if err != nil {
		t.Fatal(err)
	}
	r1 := findResult(t, rep1, ext.URL+"/soft-thing")
	if r1.Class != classify.ClassSoft404 {
		t.Fatalf("precondition: first scan should classify as soft_404, got %s (%s)", r1.Class, r1.Reason)
	}

	atomic.StoreInt32(&toggle, 1)
	e2 := NewEngine(cfg)
	e2.Cache = store
	rep2, err := e2.Run(context.Background(), seed.URL)
	if err != nil {
		t.Fatal(err)
	}
	r2 := findResult(t, rep2, ext.URL+"/soft-thing")
	if r2.Class != classify.ClassAlive {
		t.Fatalf("second scan should be fixed (alive), got %s (%s)", r2.Class, r2.Reason)
	}
	if r2.PrevClass != classify.ClassSoft404 {
		t.Errorf("PrevClass = %q, want soft_404 - this is the exact bug reported: fixed soft-404s vanish from the diff", r2.PrevClass)
	}
}

// PrevClass must track title-based soft-404 correctly across scans too, not
// just fingerprint-based ones - this is a different classification path
// with its own reason string.
func TestOscillatingTitleSoft404AcrossScans(t *testing.T) {
	var state int32 // 0=alive, 1=soft404-by-title
	ext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/thing" {
			// probe/garbage target: distinct from both /thing variants below,
			// so the fingerprint never matches - isolates the title heuristic
			html(w, `<html><head><title>Unrelated Probe Shell</title></head><body>
Completely unrelated boilerplate text used only for garbage probe responses.
</body></html>`)
			return
		}
		if atomic.LoadInt32(&state) == 0 {
			html(w, `<html><head><title>Real Article</title></head><body>
Genuine distinct content about a specific and interesting subject matter here.
</body></html>`)
		} else {
			html(w, `<html><head><title>404 Not Found - meme page</title></head><body>
This page is genuinely about the 404 meme itself and has plenty of real words.
</body></html>`)
		}
	}))
	t.Cleanup(ext.Close)

	seed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		html(w, fmt.Sprintf(`<html><body><a href="%s/thing">thing</a></body></html>`, ext.URL))
	}))
	t.Cleanup(seed.Close)

	store, err := cache.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := testConfig()
	cfg.CacheTTL = 0

	run := func(n int, wantClass classify.Class) LinkResult {
		e := NewEngine(cfg)
		e.Cache = store
		rep, err := e.Run(context.Background(), seed.URL)
		if err != nil {
			t.Fatal(err)
		}
		r := findResult(t, rep, ext.URL+"/thing")
		if r.Class != wantClass {
			t.Fatalf("scan %d: class=%s reason=%s, want %s", n, r.Class, r.Reason, wantClass)
		}
		return *r
	}

	r1 := run(1, classify.ClassAlive)
	if r1.PrevClass != "" {
		t.Errorf("scan 1: prevClass=%q, want empty (no prior record)", r1.PrevClass)
	}

	atomic.StoreInt32(&state, 1)
	r2 := run(2, classify.ClassSoft404)
	if r2.PrevClass != classify.ClassAlive {
		t.Errorf("scan 2: prevClass=%q, want alive", r2.PrevClass)
	}

	atomic.StoreInt32(&state, 0)
	r3 := run(3, classify.ClassAlive)
	if r3.PrevClass != classify.ClassSoft404 {
		t.Errorf("scan 3: prevClass=%q, want soft_404", r3.PrevClass)
	}

	atomic.StoreInt32(&state, 1)
	r4 := run(4, classify.ClassSoft404)
	if r4.PrevClass != classify.ClassAlive {
		t.Errorf("scan 4: prevClass=%q, want alive", r4.PrevClass)
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
