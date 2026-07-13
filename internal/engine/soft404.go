package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"hash/fnv"
	"math/bits"
	"regexp"
	"strings"
	"sync"
)

// simhashThreshold is the max Hamming distance (of 64 bits) at which two pages
// are considered the same template. Soft-404 pages typically differ from the
// probe response only in the echoed URL.
const simhashThreshold = 8

// fingerprints implements soft-404 detection: for each host we fetch a URL that
// cannot exist and fingerprint the response. A checked link that returns 200
// with a near-identical page is a soft 404. Hosts that answer the probe with a
// real 404, or that serve one shell for every route (SPAs), get no fingerprint.
type fingerprints struct {
	e     *Engine
	mu    sync.Mutex
	hosts map[string]*hostFP
}

type hostFP struct {
	once sync.Once
	ok   bool
	hash uint64
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
		return // host 404s properly (or is unreachable); no fingerprint needed
	}
	garbageHash := simhashHTML(garbage.Body)

	// SPA guard: if the homepage is indistinguishable from a garbage URL, the
	// host serves one shell for every route and the fingerprint would flag
	// every valid link as soft-404.
	root := e.probeFetch(ctx, host, scheme+"://"+host+"/")
	if root != nil && root.Status == 200 && root.Body != nil &&
		hamming(simhashHTML(root.Body), garbageHash) <= simhashThreshold {
		return
	}

	fp.hash = garbageHash
	fp.ok = true
}

func (fp *hostFP) matches(body []byte) bool {
	return fp.ok && len(body) > 0 && hamming(simhashHTML(body), fp.hash) <= simhashThreshold
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

// simhashHTML computes a 64-bit SimHash over 3-word shingles of the page's
// visible text, so near-identical templates hash to nearby values.
func simhashHTML(body []byte) uint64 {
	text := scriptRe.ReplaceAllString(string(body), " ")
	text = tagRe.ReplaceAllString(text, " ")
	words := strings.Fields(strings.ToLower(text))

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

func hamming(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}
