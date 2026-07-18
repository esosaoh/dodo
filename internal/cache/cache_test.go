package cache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/esosaoh/dodo/internal/classify"
)

func TestFileCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "links.json.gz")
	ctx := context.Background()

	c1, err := NewFileCache(path)
	if err != nil {
		t.Fatal(err)
	}
	in := []*LinkState{
		{URL: "https://a.example/x", Class: classify.ClassAlive, Status: 200, ETag: `"v1"`, CheckedAt: time.Now(), Successes: 1},
		{URL: "https://b.example/y", Class: classify.ClassDead, Status: 404, CheckedAt: time.Now(), Fails: 2},
	}
	if err := c1.PutStates(ctx, in); err != nil {
		t.Fatal(err)
	}

	// A fresh instance must read what the first one persisted.
	c2, err := NewFileCache(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c2.GetStates(ctx, []string{"https://a.example/x", "https://b.example/y", "https://c.example/z"})
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

func TestFileCachePrunesOldEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "links.json.gz")
	ctx := context.Background()

	c, err := NewFileCache(path)
	if err != nil {
		t.Fatal(err)
	}
	c.PutStates(ctx, []*LinkState{
		{URL: "https://old.example/", Class: classify.ClassAlive, CheckedAt: time.Now().Add(-cacheMaxAge - time.Hour)},
	})
	// Any later write triggers pruning of expired entries.
	c.PutStates(ctx, []*LinkState{
		{URL: "https://new.example/", Class: classify.ClassAlive, CheckedAt: time.Now()},
	})

	got, _ := c.GetStates(ctx, []string{"https://old.example/", "https://new.example/"})
	if _, ok := got["https://old.example/"]; ok {
		t.Error("expired entry survived pruning")
	}
	if _, ok := got["https://new.example/"]; !ok {
		t.Error("fresh entry was pruned")
	}
}

func TestFileCacheSurvivesCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "links.json.gz")
	if err := os.WriteFile(path, []byte("not gzip at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := NewFileCache(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.GetStates(context.Background(), []string{"https://a.example/"})
	if err != nil || len(got) != 0 {
		t.Errorf("corrupt cache should behave as empty, got %v (err %v)", got, err)
	}
	// And writing must recover it.
	if err := c.PutStates(context.Background(), []*LinkState{{URL: "https://a.example/", Class: classify.ClassAlive, CheckedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
}
