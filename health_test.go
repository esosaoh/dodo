package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHostHealthTripsAfterConsecutiveFailures(t *testing.T) {
	h := newHostHealth(3)
	fail := Verdict{Class: ClassDead, Reason: "connection_refused", Confidence: 0.6, Retryable: true}

	for i := 0; i < 2; i++ {
		h.record("down.example", true, fail)
	}
	if _, ok := h.trippedVerdict("down.example"); ok {
		t.Fatal("breaker tripped before reaching the limit")
	}

	// A success resets the streak.
	h.record("down.example", false, Verdict{Class: ClassAlive})
	h.record("down.example", true, fail)
	h.record("down.example", true, fail)
	if _, ok := h.trippedVerdict("down.example"); ok {
		t.Fatal("breaker did not reset on success")
	}

	h.record("down.example", true, fail)
	v, ok := h.trippedVerdict("down.example")
	if !ok {
		t.Fatal("breaker should trip at the limit")
	}
	if v.Reason != "host_unreachable" || v.Class != ClassDead || v.Retryable {
		t.Errorf("tripped verdict = %+v, want dead/host_unreachable/non-retryable", v)
	}

	if _, ok := h.trippedVerdict("other.example"); ok {
		t.Error("breaker state leaked across hosts")
	}
}

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

	rep, err := newEngine(cfg).Run(context.Background(), seed.URL)
	if err != nil {
		t.Fatal(err)
	}

	var dead, shortCircuited int
	for _, r := range rep.Results {
		if !strings.Contains(r.URL, deadHost) {
			continue
		}
		if r.Class != ClassDead {
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

func TestHostHealthBlockedStreak(t *testing.T) {
	h := newHostHealth(3)
	blocked := Verdict{Class: ClassBlocked, Reason: "forbidden", Confidence: 0.65}
	rateLimited := Verdict{Class: ClassBlocked, Reason: "rate_limited", Confidence: 0.5, Retryable: true}

	h.record("wall.example", false, blocked)
	h.record("wall.example", false, blocked)
	h.record("wall.example", false, rateLimited) // neutral: neither counts nor resets
	if _, ok := h.trippedVerdict("wall.example"); ok {
		t.Fatal("tripped early: rate_limited should not count toward the streak")
	}
	h.record("wall.example", false, blocked)
	v, ok := h.trippedVerdict("wall.example")
	if !ok || v.Class != ClassBlocked || v.Reason != "host_blocked" {
		t.Fatalf("want blocked/host_blocked trip, got %+v ok=%v", v, ok)
	}

	// An ordinary response resets the streak.
	h2 := newHostHealth(3)
	h2.record("ok.example", false, blocked)
	h2.record("ok.example", false, blocked)
	h2.record("ok.example", false, Verdict{Class: ClassAlive})
	h2.record("ok.example", false, blocked)
	if _, ok := h2.trippedVerdict("ok.example"); ok {
		t.Error("alive response should reset the blocked streak")
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

	rep, err := newEngine(cfg).Run(context.Background(), seed.URL)
	if err != nil {
		t.Fatal(err)
	}

	var blocked, shortCircuited int
	for _, r := range rep.Results {
		if !strings.Contains(r.URL, wall.URL) {
			continue
		}
		if r.Class != ClassBlocked {
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
