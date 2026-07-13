package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"hash/fnv"
	"math/bits"
	"net/url"
	"regexp"
	"strings"
	"sync"
)

// max Hamming distance (of 64 bits) at which two pages count as the same template
const simhashThreshold = 8

// fingerprints detects soft 404s: probe each host with a URL that cannot
// exist; if it answers 200, that page is the host's "not found" template and
// any checked link serving a near-identical page is a soft 404.
type fingerprints struct {
	e     *Engine
	mu    sync.Mutex
	hosts map[string]*hostFP
}

type hostFP struct {
	once sync.Once
	ok   bool
	hash uint64

	mu      sync.Mutex
	checks  int
	matched int
}

func newFingerprints(e *Engine) *fingerprints {
	return &fingerprints{e: e, hosts: make(map[string]*hostFP)}
}

func (f *fingerprints) forHost(ctx context.Context, scheme, host string) *hostFP {
	key := scheme + "://" + host
	f.mu.Lock()
	fp, ok := f.hosts[key]
	if !ok {
		fp = &hostFP{}
		f.hosts[key] = fp
	}
	f.mu.Unlock()
	fp.once.Do(func() { fp.probe(ctx, f.e, scheme, host) })
	return fp
}

func (fp *hostFP) probe(ctx context.Context, e *Engine, scheme, host string) {
	probeURL := scheme + "://" + host + "/" + randomSlug()
	garbage := e.probeFetch(ctx, host, probeURL)
	if garbage == nil || garbage.Status != 200 || garbage.Body == nil {
		return // host 404s properly; no fingerprint needed
	}
	garbageHash := simhashHTML(garbage.Body, pathTokens(probeURL))

	// SPA guard: a homepage indistinguishable from a garbage URL means one
	// shell for every route, so the fingerprint proves nothing.
	rootURL := scheme + "://" + host + "/"
	root := e.probeFetch(ctx, host, rootURL)
	if root != nil && root.Status == 200 && root.Body != nil &&
		hamming(simhashHTML(root.Body, pathTokens(rootURL)), garbageHash) <= simhashThreshold {
		return
	}

	fp.hash = garbageHash
	fp.ok = true
}

func (fp *hostFP) matches(body []byte, linkURL string) bool {
	if !fp.ok || len(body) == 0 {
		return false
	}
	matched := hamming(simhashHTML(body, pathTokens(linkURL)), fp.hash) <= simhashThreshold
	fp.mu.Lock()
	fp.checks++
	if matched {
		fp.matched++
	}
	fp.mu.Unlock()
	return matched
}

// suspect: a fingerprint matching most of a host's 200s can't be trusted.
func (fp *hostFP) suspect() bool {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return fp.checks >= 10 && fp.matched*100 >= fp.checks*60
}

func (f *fingerprints) peek(scheme, host string) *hostFP {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hosts[scheme+"://"+host]
}

func pathTokens(rawURL string) []string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	fields := strings.FieldsFunc(strings.ToLower(u.Path+" "+u.RawQuery), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	var toks []string
	for _, f := range fields {
		if len(f) >= 3 {
			toks = append(toks, f)
		}
	}
	return toks
}

func (e *Engine) probeFetch(ctx context.Context, host, url string) *FetchResult {
	if err := e.sched.Acquire(ctx, host); err != nil {
		return nil
	}
	fr := e.fetcher.Fetch(ctx, url, FetchOpts{WantBody: true})
	fb, ra := feedbackFor(fr)
	e.sched.Release(host, fb, ra)
	if fr.Err != nil || !fr.IsHTML() {
		return nil
	}
	return fr
}

func randomSlug() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "deadlink-probe-" + hex.EncodeToString(b)
}

var (
	scriptRe = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	tagRe    = regexp.MustCompile(`(?s)<[^>]*>`)

	notFoundTitleRe = regexp.MustCompile(`(?i)\b(404|not\s+found|page\s+(doesn'?t|does\s+not)\s+exist|no\s+longer\s+(available|exists))\b`)
)

func titleLooks404(title string) bool {
	return notFoundTitleRe.MatchString(title)
}

// simhashHTML hashes 3-word shingles of visible text; similar templates land
// within a few bits. Strip tokens (the URL's path words) and URL-ish words
// are dropped so pages that echo the requested URL hash identically.
func simhashHTML(body []byte, strip []string) uint64 {
	text := scriptRe.ReplaceAllString(string(body), " ")
	text = tagRe.ReplaceAllString(text, " ")
	all := strings.Fields(strings.ToLower(text))
	words := all[:0]
	for _, w := range all {
		if strings.ContainsRune(w, '/') || containsAny(w, strip) {
			continue
		}
		words = append(words, w)
	}

	var vec [64]int
	addFeature := func(s string) {
		h := fnv.New64a()
		h.Write([]byte(s))
		v := h.Sum64()
		for b := 0; b < 64; b++ {
			if v>>uint(b)&1 == 1 {
				vec[b]++
			} else {
				vec[b]--
			}
		}
	}

	if len(words) < 3 {
		addFeature(strings.Join(words, " "))
	} else {
		for i := 0; i+2 < len(words); i++ {
			addFeature(words[i] + " " + words[i+1] + " " + words[i+2])
		}
	}

	var out uint64
	for b := 0; b < 64; b++ {
		if vec[b] > 0 {
			out |= 1 << uint(b)
		}
	}
	return out
}

func containsAny(w string, toks []string) bool {
	for _, t := range toks {
		if strings.Contains(w, t) {
			return true
		}
	}
	return false
}

func hamming(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}
