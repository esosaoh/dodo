package health

import (
	"sync"
	"time"

	"github.com/esosaoh/dodo/internal/classify"
)

// rateLimitBudget: a host still 429ing this long after its first 429 (with
// AIMD and Retry-After pauses already applied) won't be reasoned with.
const rateLimitBudget = 20 * time.Second

// HostHealth holds two per-host circuit breakers: one for hosts that stopped
// answering (DNS/refused/timeout), one for hosts that keep refusing us
// (403/999-style bot walls). Either way, once tripped the host's remaining
// links inherit a verdict instead of costing a request each.
type HostHealth struct {
	mu    sync.Mutex
	hosts map[string]*hostState
	limit int
	now   func() time.Time
}

type hostState struct {
	consecFails   int
	consecBlocked int
	first429      time.Time
	budgetSpent   bool
	tripped       bool
	verdict       classify.Verdict
}

func NewHostHealth(limit int) *HostHealth {
	return &HostHealth{hosts: make(map[string]*hostState), limit: limit, now: time.Now}
}

func (h *HostHealth) TrippedVerdict(host string) (classify.Verdict, bool) {
	if h.limit <= 0 {
		return classify.Verdict{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.hosts[host]
	if !ok || !st.tripped {
		return classify.Verdict{}, false
	}
	return st.verdict, true
}

func (h *HostHealth) Record(host string, connectFailed bool, v classify.Verdict) {
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
	case v.Class == classify.ClassBlocked && v.Retryable:
		if st.first429.IsZero() {
			st.first429 = h.now()
		} else if h.now().Sub(st.first429) > rateLimitBudget {
			st.budgetSpent = true
		}
		st.consecFails = 0
	case v.Class == classify.ClassBlocked:
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
		v = classify.FinalizeVerdict(v, 3)
		v.Reason = "host_unreachable"
		st.verdict = v
	case st.consecBlocked >= h.limit:
		st.tripped = true
		st.verdict = classify.Verdict{Class: classify.ClassBlocked, Reason: "host_blocked", Confidence: 0.85}
	case st.budgetSpent:
		st.tripped = true
		st.verdict = classify.Verdict{Class: classify.ClassBlocked, Reason: "host_rate_limited", Confidence: 0.7}
	}
}
