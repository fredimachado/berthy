// Package dialtest gives tests connection failures without closing a port: a closed port can be handed to another
// test's listener before the dial, so the dial would connect.
package dialtest

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
	"testing"
)

// Refused is an address that Dial refuses as a closed port would. The .invalid name never resolves (RFC 6761), so a
// client that dials it any other way fails differently and the test notices.
const Refused = "refused.invalid:80"

// Dial refuses Refused with ECONNREFUSED without touching the network and dials every other address normally.
func Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if addr == Refused {
		return nil, &net.OpError{Op: "dial", Net: network, Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	}
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// Client returns an HTTP client that dials with Dial.
func Client() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: Dial}}
}

// HangUp serves connections that close before answering, keeping the port open until the test ends.
func HangUp(t testing.TB) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}
