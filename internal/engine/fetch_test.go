package engine

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetcherNegotiatesHTTP2(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "<html><body>proto=%s</body></html>", r.Proto)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	f := NewFetcher(testConfig())
	f.client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	fr := f.Fetch(t.Context(), srv.URL, FetchOpts{WantBody: true})
	if fr.Err != nil {
		t.Fatal(fr.Err)
	}
	if !strings.Contains(string(fr.Body), "proto=HTTP/2.0") {
		t.Errorf("expected HTTP/2 negotiation, got %s", fr.Body)
	}
}
