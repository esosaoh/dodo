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
