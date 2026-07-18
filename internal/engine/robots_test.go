package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/esosaoh/dodo/internal/classify"
)

func TestParseRobots(t *testing.T) {
	body := `
# comment
User-agent: googlebot
Disallow: /google-only/

User-agent: *
Disallow: /private/
Disallow: /tmp/*.pdf
Allow: /private/ok
Disallow: /exact$
`
	rules := parseRobots(body, "dodo/0.1 (+https://example.com)")

	cases := []struct {
		path    string
		allowed bool
	}{
		{"/", true},
		{"/google-only/x", true}, // that group isn't ours
		{"/private/secret", false},
		{"/private/ok", true},       // allow beats disallow (longer match)
		{"/tmp/report.pdf", false},  // * wildcard
		{"/tmp/report.html", true},  //
		{"/exact", false},           // $ anchor
		{"/exact-but-longer", true}, //
	}
	for _, c := range cases {
		if got := robotsAllowed(rules, c.path); got != c.allowed {
			t.Errorf("path %q: allowed=%v, want %v", c.path, got, c.allowed)
		}
	}
}

func TestParseRobotsSpecificAgentWins(t *testing.T) {
	body := `
User-agent: dodo
Disallow: /no-dodo/

User-agent: *
Disallow: /
`
	rules := parseRobots(body, "dodo/0.1")
	if !robotsAllowed(rules, "/anything") {
		t.Error("dodo group should replace * group entirely")
	}
	if robotsAllowed(rules, "/no-dodo/x") {
		t.Error("dodo group's own disallow should apply")
	}
}

func TestCrawlRespectsRobots(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "User-agent: *\nDisallow: /private/\n")
	})
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		html(w, `<html><body><a href="/open">open</a> <a href="/private/page">private</a></body></html>`)
	})
	mux.HandleFunc("/open", func(w http.ResponseWriter, r *http.Request) {
		html(w, `<html><body>fine</body></html>`)
	})
	mux.HandleFunc("/private/page", func(w http.ResponseWriter, r *http.Request) {
		html(w, `<html><body><a href="/private/leaked">must not be discovered</a></body></html>`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := testConfig()
	cfg.Soft404 = false
	rep, err := NewEngine(cfg).Run(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	// /private/page is still verified as a link (single fetch), but never
	// crawled: its outgoing links must not appear.
	if r := findResult(t, rep, srv.URL+"/private/page"); r.Class != classify.ClassAlive {
		t.Errorf("/private/page should still be verified alive, got %s", r.Class)
	}
	for _, r := range rep.Results {
		if r.URL == srv.URL+"/private/leaked" {
			t.Error("link discovered inside a robots-disallowed page")
		}
	}
	if rep.PagesCrawled != 2 { // / and /open
		t.Errorf("pages crawled = %d, want 2", rep.PagesCrawled)
	}
}

func TestCrawlIgnoresRobotsWhenDisabled(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "User-agent: *\nDisallow: /\n")
	})
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		html(w, `<html><body><a href="/a">a</a></body></html>`)
	})
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) {
		html(w, `<html><body>a</body></html>`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := testConfig()
	cfg.Soft404 = false
	cfg.RespectRobots = false
	rep, err := NewEngine(cfg).Run(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if rep.PagesCrawled != 2 {
		t.Errorf("pages crawled = %d, want 2 with robots disabled", rep.PagesCrawled)
	}
}
