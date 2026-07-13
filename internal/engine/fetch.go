package engine

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var errTooManyRedirects = errors.New("too many redirects")

type FetchOpts struct {
	WantBody     bool
	BodyCap      int64
	ETag         string
	LastModified string
}

type FetchResult struct {
	URL         string
	FinalURL    string
	Status      int
	Redirected  bool
	ContentType string
	Header      http.Header
	Body        []byte
	Err         error
	Latency     time.Duration
	RetryAfter  time.Duration
}

func (r *FetchResult) IsHTML() bool {
	return strings.Contains(r.ContentType, "text/html") ||
		strings.Contains(r.ContentType, "application/xhtml")
}

type Fetcher struct {
	client     *http.Client
	ua         string
	timeout    time.Duration
	defaultCap int64
}

func NewFetcher(cfg *Config) *Fetcher {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: cfg.Timeout,
	}
	return &Fetcher{
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errTooManyRedirects
				}
				return nil
			},
		},
		ua:         cfg.UserAgent,
		timeout:    cfg.Timeout,
		defaultCap: cfg.MaxBodyBytes,
	}
}

// Fetch GETs a URL, following redirects, reading at most the configured body cap.
// GET is used instead of HEAD because many servers answer HEAD incorrectly, and
// bodies are needed for soft-404 and fragment analysis.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string, opts FetchOpts) *FetchResult {
	res := &FetchResult{URL: rawURL, FinalURL: rawURL}

	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		res.Err = err
		return res
	}
	req.Header.Set("User-Agent", f.ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	if opts.ETag != "" {
		req.Header.Set("If-None-Match", opts.ETag)
	}
	if opts.LastModified != "" {
		req.Header.Set("If-Modified-Since", opts.LastModified)
	}

	start := time.Now()
	resp, err := f.client.Do(req)
	res.Latency = time.Since(start)
	if err != nil {
		res.Err = err
		return res
	}
	defer resp.Body.Close()

	res.Status = resp.StatusCode
	res.Header = resp.Header
	res.ContentType = strings.ToLower(resp.Header.Get("Content-Type"))
	res.FinalURL = resp.Request.URL.String()
	res.Redirected = res.FinalURL != rawURL
	res.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))

	bodyCap := opts.BodyCap
	if bodyCap == 0 {
		bodyCap = f.defaultCap
	}
	if opts.WantBody && res.IsHTML() {
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, bodyCap))
		if rerr == nil {
			res.Body = body
		}
	} else {
		// Drain a little to allow connection reuse, then bail.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 16<<10))
	}
	return res
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
