package engine

import (
	"context"
	"testing"
	"time"
)

func testScheduler() *Scheduler {
	cfg := DefaultConfig()
	cfg.PerHostInit = 4
	cfg.PerHostMax = 16
	return NewScheduler(cfg)
}

func TestSchedulerBackoffHalvesLimit(t *testing.T) {
	s := testScheduler()
	ctx := context.Background()
	if err := s.Acquire(ctx, "example.com"); err != nil {
		t.Fatal(err)
	}
	s.Release("example.com", FeedbackBackoff, 0)
	if got := s.Limit("example.com"); got != 2 {
		t.Errorf("limit after backoff = %v, want 2", got)
	}
}

func TestSchedulerGrowthIsCapped(t *testing.T) {
	s := testScheduler()
	ctx := context.Background()
	for i := 0; i < 500; i++ {
		if err := s.Acquire(ctx, "example.com"); err != nil {
			t.Fatal(err)
		}
		s.Release("example.com", FeedbackSuccess, 0)
	}
	if got := s.Limit("example.com"); got != 16 {
		t.Errorf("limit after sustained success = %v, want capped at 16", got)
	}
}

func TestSchedulerHonorsRetryAfterPause(t *testing.T) {
	s := testScheduler()
	ctx := context.Background()
	if err := s.Acquire(ctx, "example.com"); err != nil {
		t.Fatal(err)
	}
	s.Release("example.com", FeedbackBackoff, 100*time.Millisecond)

	start := time.Now()
	if err := s.Acquire(ctx, "example.com"); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited < 80*time.Millisecond {
		t.Errorf("Acquire returned after %v, expected to wait for Retry-After pause", waited)
	}
	s.Release("example.com", FeedbackNeutral, 0)
}

func TestSchedulerAcquireRespectsCancel(t *testing.T) {
	s := testScheduler()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Acquire(ctx, "example.com"); err == nil {
		t.Error("Acquire with cancelled context should error")
	}
}

func TestFeedbackForStatuses(t *testing.T) {
	cases := []struct {
		status int
		want   Feedback
	}{
		{200, FeedbackSuccess},
		{301, FeedbackSuccess},
		{403, FeedbackNeutral}, // WAF refusal must not grow the window
		{404, FeedbackNeutral},
		{429, FeedbackBackoff},
		{503, FeedbackBackoff},
		{500, FeedbackNeutral},
	}
	for _, c := range cases {
		fb, _ := feedbackFor(&FetchResult{Status: c.status, Latency: 50 * time.Millisecond})
		if fb != c.want {
			t.Errorf("status %d: feedback %v, want %v", c.status, fb, c.want)
		}
	}
}
