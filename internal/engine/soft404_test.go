package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestFingerprintSurvivesRedirectToHomepage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			html(w, `<html><head><title>Welcome</title></head><body>
Genuine homepage content about a small business that sells handmade furniture
and ships nationwide with a satisfaction guarantee on every order.
</body></html>`)
			return
		}
		// unknown paths, including our probes, redirect home
		http.Redirect(w, r, "/", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	e := NewEngine(testConfig())
	fp := e.prints.forHost(context.Background(), "http", srv.Listener.Addr().String())
	if !fp.ok {
		t.Fatal("expected a fingerprint even though the probe redirected to the homepage")
	}

	deadPageBody := []byte(`<html><head><title>Welcome</title></head><body>
Genuine homepage content about a small business that sells handmade furniture
and ships nationwide with a satisfaction guarantee on every order.
</body></html>`)
	if !fp.matches(deadPageBody, "http://"+srv.Listener.Addr().String()+"/vanished-resource") {
		t.Error("expected the fingerprint to match a link that redirects to the same homepage content")
	}
}

func TestFingerprintRejectsInconsistentGarbageResponses(t *testing.T) {
	var hits int32
	pages := []string{
		`<html><head><title>Special offer</title></head><body>
Save big this week only on our entire catalog of outdoor camping gear and
survival equipment, free shipping on every order over fifty dollars.
</body></html>`,
		`<html><head><title>Weather update</title></head><body>
Scattered showers expected this afternoon with clearing skies by evening,
temperatures dropping into the low fifties overnight across the region.
</body></html>`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := atomic.AddInt32(&hits, 1) - 1
		html(w, pages[int(i)%len(pages)])
	}))
	t.Cleanup(srv.Close)

	e := NewEngine(testConfig())
	fp := e.prints.forHost(context.Background(), "http", srv.Listener.Addr().String())
	if fp.ok {
		t.Error("expected no fingerprint when two garbage probes disagree")
	}
}

func TestSimhashSimilarPages(t *testing.T) {
	a := []byte(`<html><body>Sorry, we could not find the page you were looking for. Try searching below or return to the homepage. Error reference 8842.</body></html>`)
	b := []byte(`<html><body>Sorry, we could not find the page you were looking for. Try searching below or return to the homepage. Error reference 1391.</body></html>`)
	if d := hamming(simhashHTML(a, nil), simhashHTML(b, nil)); d > simhashThreshold {
		t.Errorf("near-identical pages should be within threshold, distance=%d", d)
	}
}

func TestSimhashDifferentPages(t *testing.T) {
	a := []byte(`<html><body>Sorry, we could not find the page you were looking for. Try searching below or return to the homepage.</body></html>`)
	b := []byte(`<html><body>Welcome to our store. Browse thousands of products across electronics, clothing, books, and garden supplies with free shipping on qualified orders today.</body></html>`)
	if d := hamming(simhashHTML(a, nil), simhashHTML(b, nil)); d <= simhashThreshold {
		t.Errorf("unrelated pages should exceed threshold, distance=%d", d)
	}
}

func TestSimhashIgnoresScripts(t *testing.T) {
	a := []byte(`<html><script>var x = 12345;</script><body>page not found anywhere</body></html>`)
	b := []byte(`<html><script>var y = 99999; totally different code;</script><body>page not found anywhere</body></html>`)
	if d := hamming(simhashHTML(a, nil), simhashHTML(b, nil)); d > simhashThreshold {
		t.Errorf("script content should not affect fingerprint, distance=%d", d)
	}
}

func TestTitleLooks404(t *testing.T) {
	yes := []string{"404 Not Found", "Page not found — Example", "This page doesn't exist"}
	no := []string{"Product catalog", "Our story", "Contact us"}
	for _, s := range yes {
		if !titleLooks404(s) {
			t.Errorf("expected %q to look like a 404 title", s)
		}
	}
	for _, s := range no {
		if titleLooks404(s) {
			t.Errorf("expected %q to not look like a 404 title", s)
		}
	}
}

func TestSimhashStripsEchoedPathTokens(t *testing.T) {
	// Hosts that echo the requested URL into an otherwise identical template
	// must fingerprint identically once path tokens are stripped.
	root := []byte(`<html><body>A perfectly healthy page with words about topic /</body></html>`)
	probe := []byte(`<html><body>A perfectly healthy page with words about topic /dodo-probe-8f3a2c91</body></html>`)
	link := []byte(`<html><body>A perfectly healthy page with words about topic /res/42</body></html>`)

	rootHash := simhashHTML(root, pathTokens("http://x/"))
	probeHash := simhashHTML(probe, pathTokens("http://x/dodo-probe-8f3a2c91"))
	linkHash := simhashHTML(link, pathTokens("http://x/res/42"))

	if d := hamming(rootHash, probeHash); d > simhashThreshold {
		t.Errorf("echo host: root vs probe distance = %d, want within threshold", d)
	}
	if d := hamming(linkHash, probeHash); d > simhashThreshold {
		t.Errorf("echo host: link vs probe distance = %d, want within threshold", d)
	}
}

func TestPathTokens(t *testing.T) {
	got := pathTokens("http://example.com/products/red-shoes-42?ref=nav")
	want := map[string]bool{"products": true, "red": true, "shoes": true, "ref": true, "nav": true}
	for _, tok := range got {
		if !want[tok] {
			t.Errorf("unexpected token %q", tok)
		}
		delete(want, tok)
	}
	if len(want) != 0 {
		t.Errorf("missing tokens: %v", want)
	}
}

func TestFingerprintSuspectSafetyNet(t *testing.T) {
	fp := &hostFP{ok: true}
	body := []byte(`<html><body>the same template every single time on this host</body></html>`)
	fp.hash = simhashHTML(body, nil)
	for i := 0; i < 10; i++ {
		fp.matches(body, "http://x/p")
	}
	if !fp.suspect() {
		t.Error("fingerprint matching 100% of responses should be suspect")
	}

	fresh := &hostFP{ok: true, hash: fp.hash}
	fresh.matches(body, "http://x/p")
	if fresh.suspect() {
		t.Error("one match should not be suspect")
	}
}
