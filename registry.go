package main

import "sync"

// link aggregates every page reference to one unique (fragment-stripped) URL.
type link struct {
	url      string
	internal bool
	refs     []Ref
}

func (l *link) fragments() []string {
	seen := make(map[string]bool)
	var out []string
	for _, r := range l.refs {
		if r.Fragment != "" && !seen[r.Fragment] {
			seen[r.Fragment] = true
			out = append(out, r.Fragment)
		}
	}
	return out
}

type registry struct {
	mu    sync.Mutex
	links map[string]*link
}

func newRegistry() *registry {
	return &registry{links: make(map[string]*link)}
}

func (r *registry) add(url string, internal bool, ref Ref) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.links[url]
	if !ok {
		l = &link{url: url, internal: internal}
		r.links[url] = l
	}
	l.refs = append(l.refs, ref)
}

func (r *registry) all() []*link {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*link, 0, len(r.links))
	for _, l := range r.links {
		out = append(out, l)
	}
	return out
}

func (r *registry) size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.links)
}
