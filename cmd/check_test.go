package cmd

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/esosaoh/dodo/internal/classify"
	"github.com/esosaoh/dodo/internal/engine"
)

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

func TestPrintChangesShowsNewAndFixed(t *testing.T) {
	prevColor := colorEnabled
	colorEnabled = false
	t.Cleanup(func() { colorEnabled = prevColor })

	rep := &engine.Report{
		Results: []engine.LinkResult{
			{URL: "https://a.example/", Class: classify.ClassDead, PrevClass: classify.ClassAlive},
			{URL: "https://b.example/", Class: classify.ClassAlive, PrevClass: classify.ClassDead},
			{URL: "https://c.example/", Class: classify.ClassAlive, PrevClass: classify.ClassAlive},
		},
	}
	out := captureStdout(t, func() { printChanges(rep) })
	if !strings.Contains(out, "newly broken") || !strings.Contains(out, "https://a.example/") {
		t.Errorf("expected newly-broken section to list a.example, got:\n%s", out)
	}
	if !strings.Contains(out, "fixed") || !strings.Contains(out, "https://b.example/") {
		t.Errorf("expected fixed section to list b.example, got:\n%s", out)
	}
	if strings.Contains(out, "c.example") {
		t.Errorf("unchanged link should not appear in the changes section, got:\n%s", out)
	}
}

func TestPrintChangesExcludesTransientErrorsFromNewlyBroken(t *testing.T) {
	rep := &engine.Report{
		Results: []engine.LinkResult{
			{URL: "https://flaky.example/", Class: classify.ClassDead, Reason: "server_error", PrevClass: classify.ClassAlive},
			{URL: "https://gone.example/", Class: classify.ClassDead, Reason: "not_found", PrevClass: classify.ClassAlive},
		},
	}
	out := captureStdout(t, func() { printChanges(rep) })
	if strings.Contains(out, "flaky.example") {
		t.Errorf("a transient server_error shouldn't be reported as newly broken, got:\n%s", out)
	}
	if !strings.Contains(out, "gone.example") {
		t.Errorf("a genuine not_found should still be reported as newly broken, got:\n%s", out)
	}
}

func TestPrintChangesSkipsWhenNothingChanged(t *testing.T) {
	rep := &engine.Report{
		Results: []engine.LinkResult{
			{URL: "https://a.example/", Class: classify.ClassAlive, PrevClass: classify.ClassAlive},
			{URL: "https://b.example/", Class: classify.ClassDead}, // no prior record
		},
	}
	out := captureStdout(t, func() { printChanges(rep) })
	if out != "" {
		t.Errorf("expected no output when nothing changed, got:\n%s", out)
	}
}

func TestPrintRefsDedupesTrailingSlash(t *testing.T) {
	prevColor := colorEnabled
	colorEnabled = false
	t.Cleanup(func() { colorEnabled = prevColor })

	refs := []engine.Ref{
		{Page: "https://endler.dev/2022/readable/", Text: "readability"},
		{Page: "https://endler.dev/2022/readable", Text: "readability"},
	}
	out := captureStdout(t, func() { printRefs(refs, "endler.dev") })
	if strings.Count(out, "readable") != 1 {
		t.Errorf("expected the trailing-slash variant to be deduped into one ref, got:\n%s", out)
	}
}

func TestPrintReportSeparatesErroringFromDead(t *testing.T) {
	prevColor := colorEnabled
	colorEnabled = false
	t.Cleanup(func() { colorEnabled = prevColor })

	rep := &engine.Report{
		Seed: "https://seed.example/",
		Results: []engine.LinkResult{
			{URL: "https://flaky.example/", Class: classify.ClassDead, Reason: "server_error", Attempts: 3},
			{URL: "https://gone.example/", Class: classify.ClassDead, Reason: "not_found"},
		},
	}
	out := captureStdout(t, func() { printReport(rep) })
	if !strings.Contains(out, "ERRORING") || !strings.Contains(out, "flaky.example") {
		t.Errorf("expected the server_error result under an ERRORING section, got:\n%s", out)
	}
	if !strings.Contains(out, "DEAD (1)") {
		t.Errorf("expected DEAD to only count the genuine not_found result, got:\n%s", out)
	}
	deadIdx := strings.Index(out, "DEAD (1)")
	erroringIdx := strings.Index(out, "ERRORING")
	if deadIdx == -1 || erroringIdx == -1 || strings.Contains(out[deadIdx:erroringIdx], "flaky.example") {
		t.Errorf("flaky.example should not appear under DEAD, got:\n%s", out)
	}
}

func TestPrintRefsShortensSameHostPages(t *testing.T) {
	prevColor := colorEnabled
	colorEnabled = false
	t.Cleanup(func() { colorEnabled = prevColor })

	refs := []engine.Ref{{Page: "https://endler.dev/2020/folklore/", Text: "story"}}
	out := captureStdout(t, func() { printRefs(refs, "endler.dev") })
	if !strings.Contains(out, "on /2020/folklore/") {
		t.Errorf("expected a same-host ref to be shortened to its path, got:\n%s", out)
	}
	if strings.Contains(out, "https://endler.dev") {
		t.Errorf("expected the seed host prefix to be dropped, got:\n%s", out)
	}
}
