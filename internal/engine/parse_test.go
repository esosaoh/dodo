package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/esosaoh/dodo/internal/classify"
)

func TestParsePageMalformedHrefs(t *testing.T) {
	doc := `<html><body>
<a href="(https://netty.io/)">paren-wrapped</a>
<a href="http://[bad">bad brackets</a>
<a href="https://good.example/x">fine</a>
<a href="mailto:a@b.c">mail</a>
<a href="javascript:void(0)">js</a>
<a href="tel:+15550100">phone</a>
</body></html>`
	pd, err := ParsePage(strings.NewReader(doc), "https://site.example/")
	if err != nil {
		t.Fatal(err)
	}
	if len(pd.Links) != 1 || pd.Links[0].URL != "https://good.example/x" {
		t.Errorf("links = %+v, want just good.example", pd.Links)
	}
	if len(pd.Malformed) != 2 {
		t.Fatalf("malformed = %+v, want the paren and bracket hrefs only", pd.Malformed)
	}
	for _, m := range pd.Malformed {
		if m.URL != "(https://netty.io/)" && m.URL != "http://[bad" {
			t.Errorf("unexpected malformed href %q", m.URL)
		}
	}
}

func TestMalformedHrefReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		html(w, `<html><body><a href="(https://oops.example/)">broken markdown</a></body></html>`)
	}))
	t.Cleanup(srv.Close)

	cfg := testConfig()
	cfg.Soft404 = false
	rep, err := NewEngine(cfg).Run(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	r := findResult(t, rep, "(https://oops.example/)")
	if r.Class != classify.ClassMalformed || r.Status != 0 {
		t.Errorf("got class=%s status=%d, want malformed with no fetch", r.Class, r.Status)
	}
	if len(r.Refs) != 1 || r.Refs[0].Text != "broken markdown" {
		t.Errorf("refs = %+v, want the referencing page with anchor text", r.Refs)
	}
	if rep.Broken != 1 {
		t.Errorf("broken = %d, want 1 (malformed counts as broken)", rep.Broken)
	}
}
