// bench serves a synthetic site with known ground truth for comparing link checkers.
//
// Layout:
//
//	:9301 site      151 pages (10ms latency), 5 links to internal 404s
//	:9302 ok        150 alive links (30ms latency)
//	:9303 soft      60 real pages + 20 soft-404s (HTTP 200 error pages, 30ms)
//	:9304 rate      60 alive links, but 429 when >4 concurrent (30ms)
//	:9305 dead      closed port -> connection refused, 30 links
//
// Ground truth: 35 dead (5 internal 404 + 30 refused), 20 soft-404, 421 alive.
package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

func page(w http.ResponseWriter, title, body string) {
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, "<html><head><title>%s</title></head><body>%s</body></html>", title, body)
}

func main() {
	deadL, err := net.Listen("tcp", "127.0.0.1:9305")
	if err != nil {
		log.Fatal(err)
	}
	deadL.Close() // guarantees connection refused on :9305

	// ok host: everything 200 after 30ms
	go func() {
		log.Fatal(http.ListenAndServe("127.0.0.1:9302", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(30 * time.Millisecond)
			page(w, "OK resource", "A perfectly healthy page with plenty of unique words about topic "+r.URL.Path)
		})))
	}()

	// soft host: /real/* fine, anything else (incl. /missing/*) is a 200 error page
	go func() {
		log.Fatal(http.ListenAndServe("127.0.0.1:9303", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(30 * time.Millisecond)
			if len(r.URL.Path) > 6 && r.URL.Path[:6] == "/real/" {
				page(w, "Real article", "Genuine long-form content that differs per page: "+r.URL.Path+" with real words about databases, travel and cooking")
				return
			}
			if r.URL.Path == "/" {
				page(w, "Soft host home", "The homepage of the soft host with its own distinct navigation, hero copy and footer full of links")
				return
			}
			page(w, "Oops", "Sorry, we could not find the page you were looking for. Try searching or head back to the homepage.")
		})))
	}()

	// rate host: 429 above 4 concurrent requests
	var inflight int64
	go func() {
		log.Fatal(http.ListenAndServe("127.0.0.1:9304", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := atomic.AddInt64(&inflight, 1)
			defer atomic.AddInt64(&inflight, -1)
			if n > 4 {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			time.Sleep(30 * time.Millisecond)
			page(w, "Rate-limited resource", "Healthy content behind a strict rate limiter "+r.URL.Path)
		})))
	}()

	// main site
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Millisecond)
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		body := ""
		for i := 0; i < 150; i++ {
			body += fmt.Sprintf(`<a href="/page/%d">page %d</a> `, i, i)
		}
		page(w, "Bench site", body)
	})
	mux.HandleFunc("/page/", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Millisecond)
		var i int
		if _, err := fmt.Sscanf(r.URL.Path, "/page/%d", &i); err != nil || i < 0 || i > 149 {
			http.NotFound(w, r)
			return
		}
		body := ""
		for j := 1; j <= 10; j++ {
			body += fmt.Sprintf(`<a href="/page/%d">page %d</a> `, (i+j)%150, (i+j)%150)
		}
		body += fmt.Sprintf(`<a href="http://127.0.0.1:9302/res/%d">ok</a> `, i)
		if i < 60 {
			body += fmt.Sprintf(`<a href="http://127.0.0.1:9303/real/%d">soft real</a> `, i)
			body += fmt.Sprintf(`<a href="http://127.0.0.1:9304/limited/%d">rate</a> `, i)
		}
		if i < 20 {
			body += fmt.Sprintf(`<a href="http://127.0.0.1:9303/missing/%d">soft 404</a> `, i)
		}
		if i < 30 {
			body += fmt.Sprintf(`<a href="http://127.0.0.1:9305/gone/%d">refused</a> `, i)
		}
		if i < 5 {
			body += fmt.Sprintf(`<a href="/broken/%d">internal 404</a> `, i)
		}
		page(w, fmt.Sprintf("Page %d", i), body)
	})

	fmt.Println("bench site ready on http://127.0.0.1:9301")
	log.Fatal(http.ListenAndServe("127.0.0.1:9301", mux))
}
