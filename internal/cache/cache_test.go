package cache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/esosaoh/dodo/internal/classify"
)

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dodo.db")
	ctx := context.Background()

	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	scan := ScanSummary{Seed: "https://a.example/", StartedAt: time.Now(), FinishedAt: time.Now(), PagesCrawled: 1, TotalLinks: 2, Broken: 1}
	in := []*LinkState{
		{URL: "https://a.example/x", Class: classify.ClassAlive, Status: 200, ETag: `"v1"`, CheckedAt: time.Now(), Successes: 1},
		{URL: "https://b.example/y", Class: classify.ClassDead, Status: 404, CheckedAt: time.Now(), Fails: 2},
	}
	if err := s1.PutStates(ctx, scan, in); err != nil {
		t.Fatal(err)
	}
	s1.Close()

	// A fresh Store on the same path must read what the first one persisted.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.GetStates(ctx, []string{"https://a.example/x", "https://b.example/y", "https://c.example/z"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d states, want 2", len(got))
	}
	a := got["https://a.example/x"]
	if a.Class != classify.ClassAlive || a.ETag != `"v1"` || a.Successes != 1 {
		t.Errorf("state mismatched after reload: %+v", a)
	}
}

func TestStoreGetStatesReturnsLatestPerURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dodo.db")
	ctx := context.Background()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	url := "https://a.example/x"
	older := time.Now().Add(-time.Hour)
	newer := time.Now()
	s.PutStates(ctx, ScanSummary{Seed: "seed", FinishedAt: time.Now()}, []*LinkState{{URL: url, Class: classify.ClassDead, CheckedAt: older}})
	s.PutStates(ctx, ScanSummary{Seed: "seed", FinishedAt: time.Now()}, []*LinkState{{URL: url, Class: classify.ClassAlive, CheckedAt: newer}})

	got, err := s.GetStates(ctx, []string{url})
	if err != nil {
		t.Fatal(err)
	}
	if got[url].Class != classify.ClassAlive {
		t.Errorf("class = %s, want the most recently checked state (alive)", got[url].Class)
	}
}

func TestStorePrunesOldScans(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dodo.db")
	ctx := context.Background()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	old := time.Now().Add(-scanMaxAge - time.Hour)
	s.PutStates(ctx, ScanSummary{Seed: "seed", FinishedAt: old}, []*LinkState{
		{URL: "https://old.example/", Class: classify.ClassAlive, CheckedAt: old},
	})
	// any later write prunes expired scans (cascades to their link_states)
	s.PutStates(ctx, ScanSummary{Seed: "seed", FinishedAt: time.Now()}, []*LinkState{
		{URL: "https://new.example/", Class: classify.ClassAlive, CheckedAt: time.Now()},
	})

	got, _ := s.GetStates(ctx, []string{"https://old.example/", "https://new.example/"})
	if _, ok := got["https://old.example/"]; ok {
		t.Error("expired scan's link state survived pruning")
	}
	if _, ok := got["https://new.example/"]; !ok {
		t.Error("fresh entry was pruned")
	}
}

func TestStoreOpenRejectsGarbageFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dodo.db")
	if err := os.WriteFile(path, []byte("not a sqlite database at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Error("expected Open to fail on a non-database file")
	}
}

func TestStoreHistoryIsChronological(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dodo.db")
	ctx := context.Background()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	url := "https://a.example/x"
	t1 := time.Now().Add(-2 * time.Hour)
	t2 := time.Now().Add(-time.Hour)
	t3 := time.Now()
	s.PutStates(ctx, ScanSummary{Seed: "seed", FinishedAt: time.Now()}, []*LinkState{{URL: url, Class: classify.ClassAlive, CheckedAt: t1}})
	s.PutStates(ctx, ScanSummary{Seed: "seed", FinishedAt: time.Now()}, []*LinkState{{URL: url, Class: classify.ClassDead, CheckedAt: t2}})
	s.PutStates(ctx, ScanSummary{Seed: "seed", FinishedAt: time.Now()}, []*LinkState{{URL: url, Class: classify.ClassAlive, CheckedAt: t3}})

	hist, err := s.History(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 3 {
		t.Fatalf("got %d history entries, want 3", len(hist))
	}
	wantClasses := []classify.Class{classify.ClassAlive, classify.ClassDead, classify.ClassAlive}
	for i, want := range wantClasses {
		if hist[i].Class != want {
			t.Errorf("history[%d].Class = %s, want %s", i, hist[i].Class, want)
		}
	}
}

func TestStoreTrendIsChronological(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dodo.db")
	ctx := context.Background()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	seed := "https://a.example/"
	s.PutStates(ctx, ScanSummary{Seed: seed, StartedAt: time.Now().Add(-time.Hour), FinishedAt: time.Now(), Broken: 5}, nil)
	s.PutStates(ctx, ScanSummary{Seed: seed, StartedAt: time.Now(), FinishedAt: time.Now(), Broken: 2}, nil)

	trend, err := s.Trend(ctx, seed)
	if err != nil {
		t.Fatal(err)
	}
	if len(trend) != 2 {
		t.Fatalf("got %d scans, want 2", len(trend))
	}
	if trend[0].Broken != 5 || trend[1].Broken != 2 {
		t.Errorf("trend = %+v, want broken counts [5, 2] in scan order", trend)
	}
}
