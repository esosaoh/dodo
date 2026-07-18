package engine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/esosaoh/dodo/internal/classify"
)

func TestCircuitBreakerShortCircuitsDeadHost(t *testing.T) {
	// A listener that is immediately closed gives instant connection-refused.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadHost := l.Addr().String()
	l.Close()

	var links strings.Builder
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&links, `<a href="http://%s/page-%d">dead %d</a>`, deadHost, i, i)
	}
	seed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		html(w, "<html><body>"+links.String()+"</body></html>")
	}))
	t.Cleanup(seed.Close)

	cfg := testConfig()
	cfg.Workers = 2
	cfg.HostFailLimit = 5
	cfg.Soft404 = false

	rep, err := NewEngine(cfg).Run(context.Background(), seed.URL)
	if err != nil {
		t.Fatal(err)
	}

	var dead, shortCircuited int
	for _, r := range rep.Results {
		if !strings.Contains(r.URL, deadHost) {
			continue
		}
		if r.Class != classify.ClassDead {
			t.Errorf("%s: class=%s (%s), want dead", r.URL, r.Class, r.Reason)
		}
		dead++
		if r.Reason == "host_unreachable" {
			shortCircuited++
		}
	}
	if dead != 12 {
		t.Errorf("found %d results for the dead host, want 12", dead)
	}
	if shortCircuited == 0 {
		t.Error("no links were short-circuited; circuit breaker never helped")
	}
}

func TestBlockedHostShortCircuit(t *testing.T) {
	wall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(wall.Close)

	var links strings.Builder
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&links, `<a href="%s/p/%d">walled %d</a>`, wall.URL, i, i)
	}
	seed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		html(w, "<html><body>"+links.String()+"</body></html>")
	}))
	t.Cleanup(seed.Close)

	cfg := testConfig()
	cfg.Workers = 2
	cfg.HostFailLimit = 5
	cfg.Soft404 = false

	rep, err := NewEngine(cfg).Run(context.Background(), seed.URL)
	if err != nil {
		t.Fatal(err)
	}

	var blocked, shortCircuited int
	for _, r := range rep.Results {
		if !strings.Contains(r.URL, wall.URL) {
			continue
		}
		if r.Class != classify.ClassBlocked {
			t.Errorf("%s: class=%s (%s), want blocked", r.URL, r.Class, r.Reason)
		}
		blocked++
		if r.Reason == "host_blocked" {
			shortCircuited++
		}
	}
	if blocked != 12 {
		t.Errorf("blocked results = %d, want 12", blocked)
	}
	if shortCircuited == 0 {
		t.Error("no links short-circuited; blocked-host breaker never tripped")
	}
}
