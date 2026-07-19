package classify

import (
	"errors"
	"net"
	"strings"
	"syscall"

	"github.com/esosaoh/dodo/internal/fetch"
)

type Class string

const (
	ClassAlive     Class = "alive"
	ClassDead      Class = "dead"
	ClassSoft404   Class = "soft_404"
	ClassMalformed Class = "malformed"
	ClassBlocked   Class = "blocked"
	ClassUnknown   Class = "unknown"
)

func Severity(c Class) int {
	switch c {
	case ClassDead:
		return 0
	case ClassSoft404:
		return 1
	case ClassMalformed:
		return 2
	case ClassBlocked:
		return 3
	case ClassUnknown:
		return 4
	default:
		return 4
	}
}

// Confidence is how sure we are of the Class, not of the link being broken.
type Verdict struct {
	Class      Class
	Reason     string
	Confidence float64
	Retryable  bool
}

func Classify(r *fetch.FetchResult) Verdict {
	if r.Err != nil {
		return ClassifyErr(r.Err)
	}
	return classifyStatus(r)
}

func ClassifyErr(err error) Verdict {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsNotFound {
			return Verdict{ClassDead, "dns_nxdomain", 0.97, false}
		}
		return Verdict{ClassUnknown, "dns_error", 0.4, true}
	}
	if errors.Is(err, fetch.ErrTooManyRedirects) {
		return Verdict{ClassDead, "redirect_loop", 0.9, false}
	}
	msg := err.Error()
	if strings.Contains(msg, "x509:") || strings.Contains(msg, "tls:") {
		return Verdict{ClassDead, "tls_error", 0.8, false}
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return Verdict{ClassDead, "connection_refused", 0.6, true}
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return Verdict{ClassUnknown, "timeout", 0.4, true}
	}
	return Verdict{ClassUnknown, "network_error", 0.4, true}
}

func classifyStatus(r *fetch.FetchResult) Verdict {
	s := r.Status
	switch {
	case s == 304:
		return Verdict{ClassAlive, "not_modified", 0.95, false}
	case s >= 200 && s < 300:
		return Verdict{ClassAlive, "", 0.95, false}
	case s >= 300 && s < 400:
		return Verdict{ClassAlive, "redirect", 0.7, false}
	}

	botProtected := looksBotProtected(r)

	switch s {
	case 400:
		return Verdict{ClassDead, "bad_request", 0.7, false}
	case 401, 407:
		return Verdict{ClassBlocked, "auth_required", 0.7, false}
	case 403:
		if botProtected {
			return Verdict{ClassBlocked, "bot_protection", 0.85, false}
		}
		return Verdict{ClassBlocked, "forbidden", 0.65, false}
	case 404:
		return Verdict{ClassDead, "not_found", 0.95, false}
	case 410:
		return Verdict{ClassDead, "gone", 0.99, false}
	case 405, 406, 501:
		return Verdict{ClassUnknown, "method_not_supported", 0.5, false}
	case 408:
		return Verdict{ClassUnknown, "timeout", 0.4, true}
	case 429:
		return Verdict{ClassBlocked, "rate_limited", 0.5, true}
	case 451:
		return Verdict{ClassBlocked, "legal", 0.9, false}
	case 999:
		return Verdict{ClassBlocked, "bot_protection", 0.9, false}
	case 500, 502, 504:
		return Verdict{ClassUnknown, "server_error", 0.55, true}
	case 503:
		if botProtected {
			return Verdict{ClassBlocked, "bot_protection", 0.85, false}
		}
		return Verdict{ClassUnknown, "server_error", 0.55, true}
	}
	switch {
	case s >= 400 && s < 500:
		return Verdict{ClassDead, "client_error", 0.6, false}
	case s >= 500:
		return Verdict{ClassUnknown, "server_error", 0.55, true}
	}
	return Verdict{ClassUnknown, "unexpected_status", 0.4, false}
}

func looksBotProtected(r *fetch.FetchResult) bool {
	if r.Header == nil {
		return false
	}
	server := strings.ToLower(r.Header.Get("Server"))
	if strings.Contains(server, "cloudflare") || strings.Contains(server, "akamai") {
		return true
	}
	if r.Header.Get("cf-mitigated") != "" || r.Header.Get("x-amzn-waf-action") != "" {
		return true
	}
	return false
}

func FinalizeVerdict(v Verdict, attempts int) Verdict {
	if !v.Retryable {
		return v
	}
	v.Retryable = false
	if attempts > 1 {
		v.Confidence = min(0.9, v.Confidence+0.15*float64(attempts-1))
	}
	switch v.Reason {
	case "server_error", "connection_refused":
		v.Class = ClassDead
	case "rate_limited":
		v.Class = ClassBlocked
	}
	return v
}
