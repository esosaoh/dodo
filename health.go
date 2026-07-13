package main

import "sync"

// hostHealth is a per-host circuit breaker: after limit consecutive
// connect-level failures, the host's remaining links inherit the failure
// verdict instead of burning a timeout each.
type hostHealth struct {
	mu    sync.Mutex
	hosts map[string]*hostState
	limit int
}

type hostState struct {
	consecFails int
	tripped     bool
	verdict     Verdict
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

// record notes one outcome; any HTTP response proves the host reachable and
// resets the streak.
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
	if !connectFailed {
		st.consecFails = 0
		return
	}
	st.consecFails++
	if st.consecFails >= h.limit && !st.tripped {
		st.tripped = true
		v = FinalizeVerdict(v, 3)
		v.Reason = "host_unreachable"
		st.verdict = v
	}
}
