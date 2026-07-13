package engine

import "testing"

func TestSimhashSimilarPages(t *testing.T) {
	a := []byte(`<html><body>Sorry, we could not find the page you were looking for. Try searching below or return to the homepage. Error reference 8842.</body></html>`)
	b := []byte(`<html><body>Sorry, we could not find the page you were looking for. Try searching below or return to the homepage. Error reference 1391.</body></html>`)
	if d := hamming(simhashHTML(a), simhashHTML(b)); d > simhashThreshold {
		t.Errorf("near-identical pages should be within threshold, distance=%d", d)
	}
}

func TestSimhashDifferentPages(t *testing.T) {
	a := []byte(`<html><body>Sorry, we could not find the page you were looking for. Try searching below or return to the homepage.</body></html>`)
	b := []byte(`<html><body>Welcome to our store. Browse thousands of products across electronics, clothing, books, and garden supplies with free shipping on qualified orders today.</body></html>`)
	if d := hamming(simhashHTML(a), simhashHTML(b)); d <= simhashThreshold {
		t.Errorf("unrelated pages should exceed threshold, distance=%d", d)
	}
}

func TestSimhashIgnoresScripts(t *testing.T) {
	a := []byte(`<html><script>var x = 12345;</script><body>page not found anywhere</body></html>`)
	b := []byte(`<html><script>var y = 99999; totally different code;</script><body>page not found anywhere</body></html>`)
	if d := hamming(simhashHTML(a), simhashHTML(b)); d > simhashThreshold {
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
