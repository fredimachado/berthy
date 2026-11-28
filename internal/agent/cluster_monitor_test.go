package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"berthy/internal/api"
	"berthy/internal/manifest"
	"berthy/internal/metrics"
	"berthy/internal/monitor"
	"berthy/internal/state"
)

func cutoverSample(t *testing.T, col *metrics.Collector) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); col.Run(ctx) }()
	synctest.Wait()
	cancel()
	<-done
	if _, ok := col.Latest(); !ok {
		t.Fatal("collector did not publish its first sample")
	}
}

func TestHostChecksBelongOnlyToTheCluster(t *testing.T) {
	setHostFiles(t, updateNotifierFiles(""))
	synctest.Test(t, func(t *testing.T) {
		sh, f, _, _ := clusterRouteFile(t)
		sh.Engine = nil
		sh.Collector = metrics.New(fixedHost{metrics.Host{Mounts: []metrics.Mount{{Path: "/", Total: 100, Available: 80}}}}, nilDocker{})
		cutoverSample(t, sh.Collector)
		h, err := Open(sh, f)
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		want := []string{"disk:/", "docker_disk", "memory", "public_ports", "reboot_required"}
		for _, r := range h.order {
			for _, check := range r.monitoring.checks(context.Background()) {
				if slices.Contains(want, check.Name()) {
					t.Errorf("%s owns host check %s", r.Config.ID(), check.Name())
				}
			}
		}
		var got []string
		for _, check := range h.cluster.checks(context.Background()) {
			got = append(got, check.Name())
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("cluster checks = %v, want %v", got, want)
		}
	})
}

func TestOnlyClusterChecksWaitForTheFirstSample(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sh, f, _, _ := clusterRouteFile(t)
		sh.Engine = nil
		sh.Collector = metrics.New(fixedHost{}, nilDocker{})
		h, err := Open(sh, f)
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		w := h.order[0].monitoring
		recordRelease(t, w.store, "first-sample", monitoredManifest(`,"probes":{"site":{"tcp":"127.0.0.1:1"}}`))
		probeAvailable := func(phase string) {
			t.Helper()
			if !slices.ContainsFunc(w.checks(context.Background()), func(c monitor.Check) bool { return c.Name() == "probe:site" }) {
				t.Errorf("environment probe unavailable %s the first host sample", phase)
			}
		}
		probeAvailable("before")
		if rows := h.order[1].monitoring.checks(context.Background()); rows == nil {
			t.Error("the empty environment returned the retry sentinel")
		}
		if got := h.cluster.checks(context.Background()); got != nil {
			t.Errorf("cluster checks were named before the sample: %v", got)
		}
		cutoverSample(t, sh.Collector)
		probeAvailable("after")
		if !slices.ContainsFunc(h.cluster.checks(context.Background()), func(c monitor.Check) bool { return c.Name() == "memory" }) {
			t.Error("the cluster did not build memory after the first sample")
		}
	})
}

type clusterDelivery struct {
	channel, host, text string
}

func clusterTestDial(deliveries chan<- clusterDelivery) monitor.DialFunc {
	return func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			r, err := http.ReadRequest(bufio.NewReader(server))
			if err != nil {
				return
			}
			defer r.Body.Close()
			body, err := io.ReadAll(r.Body)
			if err != nil {
				return
			}
			delivery := clusterDelivery{channel: "webhook", host: r.Host}
			if strings.HasSuffix(r.URL.Path, "/sendMessage") {
				form, _ := url.ParseQuery(string(body))
				delivery.channel, delivery.text = "telegram", form.Get("text")
			} else {
				var a struct{ Text string }
				if json.Unmarshal(body, &a) != nil {
					return
				}
				delivery.text = a.Text
			}
			select {
			case deliveries <- delivery:
			default:
				return
			}
			if r.Host == "failed.example.test" {
				_, _ = io.WriteString(server, "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\n\r\n")
				return
			}
			_, _ = io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Length: 11\r\n\r\n{\"ok\":true}")
		}()
		return client, nil
	}
}

func openCutoverCluster(t *testing.T, sh Shared, f File, deliveries chan<- clusterDelivery) *Cluster {
	t.Helper()
	c, err := OpenCluster(sh, f)
	if err != nil {
		t.Fatal(err)
	}
	if deliveries != nil {
		c.alertConfig.dial = clusterTestDial(deliveries)
	}
	return c
}

func cutoverCheck(t *testing.T, c *Cluster, name string) state.CheckState {
	t.Helper()
	rows, err := c.store.Checks(context.Background())
	if err == nil {
		for _, row := range rows {
			if row.Name == name {
				return row
			}
		}
	}
	t.Fatalf("no stored cluster %s: %v, rows %v", name, err, rows)
	return state.CheckState{}
}

func cutoverMessages(deliveries chan clusterDelivery) []clusterDelivery {
	var messages []clusterDelivery
	for len(deliveries) != 0 {
		messages = append(messages, <-deliveries)
	}
	return messages
}

func cutoverCredential(t *testing.T, c ClusterConfig, name, value string) {
	t.Helper()
	if err := WriteClusterSecret(context.Background(), c, name, []byte(value)); err != nil {
		t.Fatal(err)
	}
}

func TestHostWarningSurvivesRestartAndRecoversOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, f, _, _ := clusterRouteFile(t)
		f.TelegramAPIURL = "http://127.0.0.1:19000"
		f.Cluster.Alerts = &ClusterAlerts{Reminder: "0s", Telegram: &manifest.Telegram{BotTokenSecret: "TG", ChatID: "1"}, Webhook: &manifest.Webhook{URLSecret: "HOOK"}}
		deliveries := make(chan clusterDelivery, 16)
		initial := openCutoverCluster(t, Shared{}, f, deliveries)
		initial.Close()
		cutoverCredential(t, f.Cluster, "TG", "fixture-token")
		cutoverCredential(t, f.Cluster, "HOOK", "http://fixture.example.test/hook")
		for _, tc := range []struct {
			packages, detail, notifiedState string
			count                           int
		}{
			{"first", "reboot required for first", monitor.Warning, 2},
			{"second", "reboot required for second", monitor.Warning, 0},
			{"", "no reboot pending", monitor.OK, 2},
		} {
			files := updateNotifierFiles("")
			if tc.packages != "" {
				files["var/run/reboot-required"], files["var/run/reboot-required.pkgs"] = "", tc.packages
			}
			setHostFiles(t, files)
			func() {
				c := openCutoverCluster(t, Shared{}, f, deliveries)
				defer c.Close()
				c.Start(context.Background())
				synctest.Wait()
				row := cutoverCheck(t, c, "reboot_required")
				if row.Detail != tc.detail || row.NotifiedState != tc.notifiedState {
					t.Fatalf("reboot check = %+v, want detail %q and notification %q", row, tc.detail, tc.notifiedState)
				}
				got := cutoverMessages(deliveries)
				var channels []string
				for _, delivery := range got {
					channels = append(channels, delivery.channel)
					if !strings.Contains(delivery.text, "server "+c.hostname+" reboot_required") {
						t.Errorf("wrong delivery: %+v", delivery)
					}
				}
				slices.Sort(channels)
				if len(got) != tc.count || tc.count != 0 && !slices.Equal(channels, []string{"telegram", "webhook"}) {
					t.Errorf("%s: got %v, want %d deliveries", tc.detail, got, tc.count)
				}
			}()
		}
	})
}

func TestSetupCredentialCorrectionRetriesPendingHostWarning(t *testing.T) {
	files := updateNotifierFiles("")
	files["var/run/reboot-required"] = ""
	setHostFiles(t, files)
	synctest.Test(t, func(t *testing.T) {
		_, f, _, _ := clusterRouteFile(t)
		f.Cluster.Alerts = &ClusterAlerts{Reminder: "0s", Webhook: &manifest.Webhook{URLSecret: "HOOK"}}
		deliveries := make(chan clusterDelivery, 16)
		logs := make(chan string, 16)
		sh := Shared{Log: func(s string) {
			select {
			case logs <- s:
			default:
			}
		}}
		initial := openCutoverCluster(t, sh, f, deliveries)
		initial.Close()
		cutoverCredential(t, f.Cluster, "HOOK", "http://failed.example.test/hook")
		begin := time.Now()
		for i, want := range []string{"failed.example.test", "fixed.example.test", ""} {
			if i > 0 {
				cutoverCredential(t, f.Cluster, "HOOK", "http://fixed.example.test/hook")
			}
			func() {
				c := openCutoverCluster(t, sh, f, deliveries)
				defer c.Close()
				c.Start(context.Background())
				synctest.Wait()
				got := cutoverMessages(deliveries)
				if want == "" && len(got) != 0 || want != "" && (len(got) != 1 || got[0].host != want) {
					t.Fatalf("start %d: sends %+v, want one to %q when nonempty", i+1, got, want)
				}
				row := cutoverCheck(t, c, "reboot_required")
				if i == 0 {
					select {
					case msg := <-logs:
						if !strings.Contains(msg, "alert not sent for reboot_required: webhook: webhook returned HTTP 503") {
							t.Fatalf("failure log = %q", msg)
						}
					default:
						t.Fatal("delivery failure was not finished and logged")
					}
					if row.NotifiedState != "" || !row.Notified.IsZero() {
						t.Fatalf("failed warning counted as delivered: %+v", row)
					}
				} else if row.NotifiedState != monitor.Warning || time.Since(begin) >= 30*time.Second {
					t.Fatalf("pending warning did not deliver on first evaluation: %+v after %s", row, time.Since(begin))
				}
			}()
		}
	})
}

func TestClusterRecordsChecksWithoutUsableChannels(t *testing.T) {
	setHostFiles(t, updateNotifierFiles(""))
	synctest.Test(t, func(t *testing.T) {
		_, f, ids, readers := clusterRouteFile(t)
		for _, alerts := range []*ClusterAlerts{nil, {Webhook: &manifest.Webhook{URLSecret: "HOOK"}}} {
			f.Cluster.Alerts = alerts
			func() {
				c := openCutoverCluster(t, Shared{}, f, nil)
				defer c.Close()
				c.Start(context.Background())
				synctest.Wait()
				if row := cutoverCheck(t, c, "reboot_required"); row.State != monitor.OK {
					t.Errorf("reboot with no usable channel: %+v", row)
				}
				if alerts != nil {
					if row := cutoverCheck(t, c, "webhook"); row.State != monitor.Warning || row.NotifiedState != "" {
						t.Errorf("missing webhook: %+v", row)
					}
				}
			}()
		}
		cutoverCredential(t, f.Cluster, "HOOK", "http://fixture.example.test/hook")
		h, err := Open(Shared{}, f)
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		w := serve(h, "GET", ids[0].APIPath()+"doctor", readers[0], "")
		var body struct{ Checks []api.Check }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("doctor: %d %s", w.Code, w.Body.String())
		}
		if check, found := findCheck(body.Checks, "cluster_webhook"); !found || !check.OK || check.Critical {
			t.Errorf("doctor readiness: %+v", check)
		}
	})
}

type blockingHostFiles struct {
	fs.FS
	read, release chan struct{}
}

func (f blockingHostFiles) Open(name string) (fs.File, error) {
	if name == "proc/net/tcp" {
		close(f.read)
		<-f.release
	}
	return f.FS.Open(name)
}

func TestHostCloseDrainsClusterMonitorBeforeClosingItsStore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, f, _, _ := clusterRouteFile(t)
		unstarted := openCutoverCluster(t, Shared{}, f, nil)
		(&Host{cluster: unstarted}).Close()
		if _, err := unstarted.store.Checks(context.Background()); !errors.Is(err, state.ErrClosed) {
			t.Fatalf("close-before-start left the store open: %v", err)
		}
		c := openCutoverCluster(t, Shared{}, f, nil)
		h := &Host{cluster: c}
		finished := make(chan struct{})
		read, release := make(chan struct{}), make(chan struct{})
		unblock := sync.OnceFunc(func() { close(release) })
		saved := hostFiles
		hostFiles = blockingHostFiles{FS: saved, read: read, release: release}
		defer func() { hostFiles = saved }()
		defer func() {
			unblock()
			if finished != nil {
				h.Close()
			}
		}()
		if err := h.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		select {
		case <-read:
		default:
			t.Fatal("Host.Start did not start the cluster monitor")
		}
		done := finished
		go func() { h.Close(); close(done) }()
		finished = nil
		defer func() { unblock(); <-done }()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("Host.Close returned while the host check was blocked")
		default:
		}
		if _, err := c.store.Checks(context.Background()); err != nil {
			t.Errorf("cluster store closed before the worker drained: %v", err)
		}
		unblock()
		<-done
		if _, err := c.store.Checks(context.Background()); !errors.Is(err, state.ErrClosed) {
			t.Errorf("cluster store remained open after shutdown: %v", err)
		}
	})
}
