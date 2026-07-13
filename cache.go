package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// entries unrefreshed this long are pruned on save
const cacheMaxAge = 90 * 24 * time.Hour

// fileCache backs incremental re-scans: a gzipped JSON map of link states.
type fileCache struct {
	path string

	mu     sync.Mutex
	states map[string]*LinkState
	loaded bool
}

// path "" means the default under os.UserCacheDir
func newFileCache(path string) (*fileCache, error) {
	if path == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("resolve cache dir: %w", err)
		}
		path = filepath.Join(dir, "dodo", "links.json.gz")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	return &fileCache{path: path}, nil
}

func (c *fileCache) load() error {
	if c.loaded {
		return nil
	}
	c.loaded = true
	c.states = make(map[string]*LinkState)

	f, err := os.Open(c.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err // corrupt cache; start fresh
	}
	defer zr.Close()
	return json.NewDecoder(zr).Decode(&c.states)
}

func (c *fileCache) GetStates(_ context.Context, urls []string) (map[string]*LinkState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.load(); err != nil {
		return map[string]*LinkState{}, nil // unreadable cache is not fatal
	}
	out := make(map[string]*LinkState)
	for _, u := range urls {
		if s, ok := c.states[u]; ok {
			cp := *s
			out[u] = &cp
		}
	}
	return out, nil
}

func (c *fileCache) PutStates(_ context.Context, states []*LinkState) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	for _, s := range states {
		cp := *s
		c.states[s.URL] = &cp
	}
	cutoff := time.Now().Add(-cacheMaxAge)
	for url, s := range c.states {
		if s.CheckedAt.Before(cutoff) {
			delete(c.states, url)
		}
	}
	return c.save()
}

// temp file + rename, so an interrupted scan can't corrupt the cache
func (c *fileCache) save() error {
	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".links-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	zw := gzip.NewWriter(tmp)
	if err := json.NewEncoder(zw).Encode(c.states); err != nil {
		tmp.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), c.path)
}
