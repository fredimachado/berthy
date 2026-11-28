package agentclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"
)

// ConnectionMessage describes a transport failure without exposing the request URL or credentials.
func ConnectionMessage(base *url.URL, err error, redact func(string) string) string {
	port := base.Port()
	if port == "" {
		port = "80"
	}
	reason := ""
	var netErr net.Error
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout():
		reason = "connection timed out"
	case errors.As(err, &dnsErr):
		if dnsErr.IsNotFound {
			reason = "name not found"
		} else {
			reason = "DNS lookup failed"
		}
	case errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.Errno(10061)):
		reason = "connection refused"
	case errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.Errno(10051)) || errors.Is(err, syscall.Errno(10065)):
		reason = "network unreachable"
	default:
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		reason = err.Error()
	}
	return TerminalSafe(redact(fmt.Sprintf("agent connection failed at %s: %s", net.JoinHostPort(base.Hostname(), port), reason)))
}

// TerminalSafe makes control characters visible without allowing terminal rewriting.
func TerminalSafe(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteString("    ")
		case r == '\n' || r == '\r':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
