package main

import "sync"

// hostHealth holds two per-host circuit breakers: one for hosts that stopped
// answering (DNS/refused/timeout), one for hosts that keep refusing us
// (403/999-style bot walls). Either way, once tripped the host's remaining
// links inherit a verdict instead of costing a request each.
type hostHealth struct {
	mu    sync.Mutex
	hosts map[string]*hostState
	limit int
}

type hostState struct {
	consecFails   int
	consecBlocked int
	rateLimitHits int
	tripped       bool
	verdict       Verdict
}

func newHostHealth(limit int) *hostHealth {
	return &hostHealth{hosts: make(map[string]*hostState), limit: limit}
}

func (h *hostHealth) trippedVerdict(host string) (Verdict, bool) {
	if h.limit <= 0 {
		return Verdict{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.hosts[host]
	if !ok || !st.tripped {
		return Verdict{}, false
	}
	return st.verdict, true
}

func (h *hostHealth) record(host string, connectFailed bool, v Verdict) {
	if h.limit <= 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.hosts[host]
	if !ok {
		st = &hostState{}
		h.hosts[host] = st
	}

	switch {
	case connectFailed:
		// a connect failure doesn't reset the blocked streak: the host still
		// isn't serving us
		st.consecFails++
	case v.Class == ClassBlocked && v.Retryable:
		// 429s don't build the blocked streak, but a host that keeps
		// rate-limiting us across pause cycles gets a cumulative budget:
		// verifying its links politely would take minutes we don't spend
		st.rateLimitHits++
		st.consecFails = 0
	case v.Class == ClassBlocked:
		st.consecBlocked++
		st.consecFails = 0
	default:
		st.consecFails = 0
		st.consecBlocked = 0
	}

	if st.tripped {
		return
	}
	switch {
	case st.consecFails >= h.limit:
		st.tripped = true
		v = FinalizeVerdict(v, 3)
		v.Reason = "host_unreachable"
		st.verdict = v
	case st.consecBlocked >= h.limit:
		st.tripped = true
		st.verdict = Verdict{Class: ClassBlocked, Reason: "host_blocked", Confidence: 0.85}
	case st.rateLimitHits >= 3:
		st.tripped = true
		st.verdict = Verdict{Class: ClassBlocked, Reason: "host_rate_limited", Confidence: 0.7}
	}
}
