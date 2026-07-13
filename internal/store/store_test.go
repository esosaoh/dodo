package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/esosaoh/crawl/internal/engine"
)

func testStore(t *testing.T) Store {
	t.Helper()
	if uri := os.Getenv("MONGO_TEST_URI"); uri != "" {
		m, err := NewMongo(context.Background(), uri, "deadlink_test")
		if err != nil {
			t.Fatalf("connecting to test mongo: %v", err)
		}
		t.Cleanup(func() { m.Close(context.Background()) })
		return m
	}
	return NewMemory()
}

func TestLinkStateRoundTrip(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	in := []*engine.LinkState{
		{URL: "https://a.example/x", Class: engine.ClassAlive, Status: 200, ETag: `"v1"`, CheckedAt: time.Now().Truncate(time.Millisecond), Successes: 1},
		{URL: "https://b.example/y", Class: engine.ClassDead, Status: 404, CheckedAt: time.Now().Truncate(time.Millisecond), Fails: 3},
	}
	if err := st.PutStates(ctx, in); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetStates(ctx, []string{"https://a.example/x", "https://b.example/y", "https://c.example/z"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d states, want 2", len(got))
	}
	a := got["https://a.example/x"]
	if a == nil || a.Class != engine.ClassAlive || a.ETag != `"v1"` || a.Successes != 1 {
		t.Errorf("state a mismatched: %+v", a)
	}

	// Upsert must overwrite, not duplicate.
	in[0].Successes = 2
	if err := st.PutStates(ctx, in[:1]); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetStates(ctx, []string{"https://a.example/x"})
	if got["https://a.example/x"].Successes != 2 {
		t.Errorf("upsert did not overwrite: %+v", got["https://a.example/x"])
	}
}

func TestScanRecordLifecycle(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	rec := &ScanRecord{ID: "t1", Seed: "https://example.com/", Status: ScanRunning, CreatedAt: time.Now().Truncate(time.Millisecond)}
	if err := st.CreateScan(ctx, rec); err != nil {
		t.Fatal(err)
	}
	rec.Status = ScanDone
	rec.Report = &engine.Report{Seed: rec.Seed, Broken: 1, Counts: map[engine.Class]int{engine.ClassDead: 1}}
	if err := st.UpdateScan(ctx, rec); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetScan(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ScanDone || got.Report == nil || got.Report.Broken != 1 {
		t.Errorf("scan record mismatched: %+v", got)
	}

	if _, err := st.GetScan(ctx, "nope"); err != ErrNotFound {
		t.Errorf("missing scan: got err %v, want ErrNotFound", err)
	}

	list, err := st.ListScans(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 {
		t.Error("ListScans returned nothing")
	}
}
