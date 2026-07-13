package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/esosaoh/crawl/internal/engine"
	"github.com/esosaoh/crawl/internal/store"
	"github.com/esosaoh/crawl/web"
)

type Server struct {
	store store.Store
	cfg   *engine.Config

	mu   sync.Mutex
	jobs map[string]*job
}

// job tracks one running scan and fans progress out to SSE subscribers.
type job struct {
	id     string
	seed   string
	status store.ScanStatus

	mu       sync.Mutex
	progress engine.Progress
	subs     map[chan engine.Progress]struct{}
	done     chan struct{}
}

func NewServer(st store.Store, cfg *engine.Config) *Server {
	return &Server{store: st, cfg: cfg, jobs: make(map[string]*job)}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/scans", s.handleCreateScan)
	mux.HandleFunc("GET /api/scans", s.handleListScans)
	mux.HandleFunc("GET /api/scans/{id}", s.handleGetScan)
	mux.HandleFunc("GET /api/scans/{id}/events", s.handleScanEvents)

	static, err := fs.Sub(web.Files, ".")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /", http.FileServerFS(static))
	return mux
}

type createScanRequest struct {
	URL           string `json:"url"`
	MaxDepth      *int   `json:"max_depth,omitempty"`
	MaxPages      *int   `json:"max_pages,omitempty"`
	CheckExternal *bool  `json:"check_external,omitempty"`
	Soft404       *bool  `json:"soft404,omitempty"`
	Fragments     *bool  `json:"fragments,omitempty"`
}

func (s *Server) handleCreateScan(w http.ResponseWriter, r *http.Request) {
	var req createScanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.URL == "" {
		httpError(w, http.StatusBadRequest, "url is required")
		return
	}

	cfg := *s.cfg
	if req.MaxDepth != nil {
		cfg.MaxDepth = clamp(*req.MaxDepth, 0, 10)
	}
	if req.MaxPages != nil {
		cfg.MaxPages = clamp(*req.MaxPages, 1, 10000)
	}
	if req.CheckExternal != nil {
		cfg.CheckExternal = *req.CheckExternal
	}
	if req.Soft404 != nil {
		cfg.Soft404 = *req.Soft404
	}
	if req.Fragments != nil {
		cfg.CheckFragments = *req.Fragments
	}

	j := &job{
		id:     newID(),
		seed:   req.URL,
		status: store.ScanRunning,
		subs:   make(map[chan engine.Progress]struct{}),
		done:   make(chan struct{}),
	}
	s.mu.Lock()
	s.jobs[j.id] = j
	s.mu.Unlock()

	rec := &store.ScanRecord{
		ID:        j.id,
		Seed:      req.URL,
		Status:    store.ScanRunning,
		CreatedAt: time.Now(),
	}
	if err := s.store.CreateScan(r.Context(), rec); err != nil {
		httpError(w, http.StatusInternalServerError, "failed to persist scan")
		return
	}

	go s.runScan(j, &cfg, rec)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"id": j.id})
}

func (s *Server) runScan(j *job, cfg *engine.Config, rec *store.ScanRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	e := engine.New(cfg)
	e.Cache = s.store
	e.OnProgress = j.broadcast

	rep, err := e.Run(ctx, j.seed)
	if err != nil {
		rec.Status = store.ScanFailed
		rec.Error = err.Error()
		j.setStatus(store.ScanFailed)
	} else {
		rec.Status = store.ScanDone
		rec.Report = rep
		j.setStatus(store.ScanDone)
	}
	if uerr := s.store.UpdateScan(context.Background(), rec); uerr != nil {
		log.Printf("scan %s: persisting result failed: %v", j.id, uerr)
	}
	close(j.done)

	// Keep the finished job around briefly so late SSE connects still resolve,
	// then let the store be the source of truth.
	time.AfterFunc(time.Minute, func() {
		s.mu.Lock()
		delete(s.jobs, j.id)
		s.mu.Unlock()
	})
}

func (s *Server) handleListScans(w http.ResponseWriter, r *http.Request) {
	recs, err := s.store.ListScans(r.Context(), 50)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to list scans")
		return
	}
	writeJSON(w, recs)
}

func (s *Server) handleGetScan(w http.ResponseWriter, r *http.Request) {
	rec, err := s.store.GetScan(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such scan")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to load scan")
		return
	}
	writeJSON(w, rec)
}

func (s *Server) handleScanEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	j := s.jobs[id]
	s.mu.Unlock()

	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	if j == nil {
		// Already finished (or unknown): tell the client to fetch the record.
		fmt.Fprintf(w, "event: done\ndata: {}\n\n")
		flusher.Flush()
		return
	}

	ch := j.subscribe()
	defer j.unsubscribe(ch)

	send := func(p engine.Progress) {
		data, _ := json.Marshal(p)
		fmt.Fprintf(w, "event: progress\ndata: %s\n\n", data)
		flusher.Flush()
	}
	send(j.snapshot())

	for {
		select {
		case <-r.Context().Done():
			return
		case <-j.done:
			fmt.Fprintf(w, "event: done\ndata: {}\n\n")
			flusher.Flush()
			return
		case p := <-ch:
			send(p)
		}
	}
}

func (j *job) subscribe() chan engine.Progress {
	ch := make(chan engine.Progress, 16)
	j.mu.Lock()
	j.subs[ch] = struct{}{}
	j.mu.Unlock()
	return ch
}

func (j *job) unsubscribe(ch chan engine.Progress) {
	j.mu.Lock()
	delete(j.subs, ch)
	j.mu.Unlock()
}

func (j *job) snapshot() engine.Progress {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.progress
}

func (j *job) setStatus(st store.ScanStatus) {
	j.mu.Lock()
	j.status = st
	j.mu.Unlock()
}

func (j *job) broadcast(p engine.Progress) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.progress = p
	for ch := range j.subs {
		select {
		case ch <- p:
		default: // slow subscriber; it will catch up from a later snapshot
		}
	}
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
