package health

import (
	"testing"
	"time"

	"github.com/esosaoh/dodo/internal/classify"
)

func TestHostHealthTripsAfterConsecutiveFailures(t *testing.T) {
	h := NewHostHealth(3)
	fail := classify.Verdict{Class: classify.ClassDead, Reason: "connection_refused", Confidence: 0.6, Retryable: true}

	for i := 0; i < 2; i++ {
		h.Record("down.example", true, fail)
	}
	if _, ok := h.TrippedVerdict("down.example"); ok {
		t.Fatal("breaker tripped before reaching the limit")
	}

	// A success resets the streak.
	h.Record("down.example", false, classify.Verdict{Class: classify.ClassAlive})
	h.Record("down.example", true, fail)
	h.Record("down.example", true, fail)
	if _, ok := h.TrippedVerdict("down.example"); ok {
		t.Fatal("breaker did not reset on success")
	}

	h.Record("down.example", true, fail)
	v, ok := h.TrippedVerdict("down.example")
	if !ok {
		t.Fatal("breaker should trip at the limit")
	}
	if v.Reason != "host_unreachable" || v.Class != classify.ClassDead || v.Retryable {
		t.Errorf("tripped verdict = %+v, want dead/host_unreachable/non-retryable", v)
	}

	if _, ok := h.TrippedVerdict("other.example"); ok {
		t.Error("breaker state leaked across hosts")
	}
}

func TestHostHealthBlockedStreak(t *testing.T) {
	h := NewHostHealth(3)
	blocked := classify.Verdict{Class: classify.ClassBlocked, Reason: "forbidden", Confidence: 0.65}
	rateLimited := classify.Verdict{Class: classify.ClassBlocked, Reason: "rate_limited", Confidence: 0.5, Retryable: true}

	h.Record("wall.example", false, blocked)
	h.Record("wall.example", false, blocked)
	h.Record("wall.example", false, rateLimited) // neutral: neither counts nor resets
	if _, ok := h.TrippedVerdict("wall.example"); ok {
		t.Fatal("tripped early: rate_limited should not count toward the streak")
	}
	h.Record("wall.example", false, blocked)
	v, ok := h.TrippedVerdict("wall.example")
	if !ok || v.Class != classify.ClassBlocked || v.Reason != "host_blocked" {
		t.Fatalf("want blocked/host_blocked trip, got %+v ok=%v", v, ok)
	}

	// An ordinary response resets the streak.
	h2 := NewHostHealth(3)
	h2.Record("ok.example", false, blocked)
	h2.Record("ok.example", false, blocked)
	h2.Record("ok.example", false, classify.Verdict{Class: classify.ClassAlive})
	h2.Record("ok.example", false, blocked)
	if _, ok := h2.TrippedVerdict("ok.example"); ok {
		t.Error("alive response should reset the blocked streak")
	}
}

func TestHostHealthRateLimitBudget(t *testing.T) {
	h := NewHostHealth(5)
	clock := time.Now()
	h.now = func() time.Time { return clock }
	rl := classify.Verdict{Class: classify.ClassBlocked, Reason: "rate_limited", Confidence: 0.5, Retryable: true}

	// 429s within the budget window are patience, not a verdict.
	h.Record("wall429.example", false, rl)
	clock = clock.Add(5 * time.Second)
	h.Record("wall429.example", false, rl)
	if _, ok := h.TrippedVerdict("wall429.example"); ok {
		t.Fatal("tripped inside the rate-limit budget")
	}

	// Still 429ing after the budget: give up on the host.
	clock = clock.Add(rateLimitBudget)
	h.Record("wall429.example", false, rl)
	v, ok := h.TrippedVerdict("wall429.example")
	if !ok || v.Reason != "host_rate_limited" || v.Class != classify.ClassBlocked {
		t.Fatalf("want blocked/host_rate_limited, got %+v ok=%v", v, ok)
	}

	// A host that stopped 429ing (pacing worked) never trips, no matter how
	// long ago its first 429 was.
	h2 := NewHostHealth(5)
	clock2 := time.Now()
	h2.now = func() time.Time { return clock2 }
	h2.Record("pacing.example", false, rl)
	clock2 = clock2.Add(2 * rateLimitBudget)
	for i := 0; i < 50; i++ {
		h2.Record("pacing.example", false, classify.Verdict{Class: classify.ClassAlive})
	}
	if _, ok := h2.TrippedVerdict("pacing.example"); ok {
		t.Fatal("host that recovered from 429s must not trip")
	}
}
