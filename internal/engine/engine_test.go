package engine

import (
	"berthy/internal/api"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestSelect(t *testing.T) {
	a := []api.App{{Ref: api.AppRef{Kind: "swarm", Name: "web"}}, {Ref: api.AppRef{Kind: "compose", Name: "web"}}}
	for _, c := range []struct {
		s    string
		code int
	}{{"web", 409}, {"swarm:web", 200}, {"missing", 404}} {
		_, got := Select(a, c.s)
		if got != c.code {
			t.Errorf("%s: %d", c.s, got)
		}
	}
}

func TestStatsDecodesDockerCounters(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "stream=false&one-shot=true" {
			t.Errorf("query %s", r.URL.RawQuery)
		}
		fmt.Fprint(w, `{"cpu_stats":{"cpu_usage":{"total_usage":100,"percpu_usage":[1,2]},"system_cpu_usage":500,"online_cpus":2},"memory_stats":{"usage":1000,"limit":2000,"stats":{"inactive_file":100}},"networks":{"eth0":{"rx_bytes":3,"tx_bytes":4}},"blkio_stats":{"io_service_bytes_recursive":[{"op":"Read","value":7}]}}`)
	}))
	defer s.Close()
	e := &Engine{client: &http.Client{Transport: rewriteTransport{base: s.URL}}}
	v, err := e.Stats(context.Background(), "abc")
	if err != nil || v.CPUUsage != 100 || v.SystemUsage != 500 || v.Cache != 100 || v.NetworkRXBytes != 3 || v.BlockReadBytes != 7 {
		t.Fatalf("stats %+v %v", v, err)
	}
}

type rewriteTransport struct{ base string }

func (t rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	u, _ := url.Parse(t.base)
	r.URL.Scheme = u.Scheme
	r.URL.Host = u.Host
	return http.DefaultTransport.RoundTrip(r)
}
