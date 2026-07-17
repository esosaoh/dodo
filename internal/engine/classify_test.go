package engine

import (
	"net"
	"net/http"
	"testing"
)

func TestClassifyStatus(t *testing.T) {
	cases := []struct {
		status    int
		header    http.Header
		class     Class
		retryable bool
	}{
		{200, nil, ClassAlive, false},
		{204, nil, ClassAlive, false},
		{304, nil, ClassAlive, false},
		{400, nil, ClassDead, false},
		{403, nil, ClassBlocked, false},
		{403, http.Header{"Server": {"cloudflare"}}, ClassBlocked, false},
		{404, nil, ClassDead, false},
		{410, nil, ClassDead, false},
		{418, nil, ClassDead, false},
		{429, nil, ClassBlocked, true},
		{451, nil, ClassBlocked, false},
		{500, nil, ClassUnknown, true},
		{502, nil, ClassUnknown, true},
		{503, nil, ClassUnknown, true},
		{503, http.Header{"Server": {"cloudflare"}}, ClassBlocked, false},
		{999, nil, ClassBlocked, false},
	}
	for _, c := range cases {
		v := Classify(&FetchResult{Status: c.status, Header: c.header})
		if v.Class != c.class || v.Retryable != c.retryable {
			t.Errorf("status %d: got (%s, retryable=%v), want (%s, retryable=%v)",
				c.status, v.Class, v.Retryable, c.class, c.retryable)
		}
	}
}

func TestClassifyDNSNotFound(t *testing.T) {
	err := &net.DNSError{IsNotFound: true}
	v := Classify(&FetchResult{Err: err})
	if v.Class != ClassDead || v.Reason != "dns_nxdomain" || v.Retryable {
		t.Errorf("got %+v, want dead/dns_nxdomain/non-retryable", v)
	}
}

func TestFinalizeVerdictEscalation(t *testing.T) {
	v := FinalizeVerdict(Verdict{ClassUnknown, "server_error", 0.55, true}, 3)
	if v.Class != ClassDead {
		t.Errorf("persistent server_error should finalize to dead, got %s", v.Class)
	}
	if v.Confidence <= 0.55 {
		t.Errorf("confidence should rise with repeated failures, got %v", v.Confidence)
	}
	if v.Retryable {
		t.Error("finalized verdict must not be retryable")
	}

	v = FinalizeVerdict(Verdict{ClassAlive, "", 0.95, false}, 1)
	if v.Class != ClassAlive || v.Confidence != 0.95 {
		t.Errorf("non-retryable verdict should pass through, got %+v", v)
	}
}
