// Command fakecloudflare serves the in-memory Cloudflare API from internal/cloudflare/cftest on a loopback address, so
// managed ingress can be exercised end to end without Cloudflare. It is a test tool. The agent and the CLI never import
// it, so it is not part of the production binaries.
//
// The Cloudflare API is served under /client/v4, which is the agent's cloudflare_api_url. A control plane under /_fake/
// seeds and inspects the account. The control plane has no authentication, so the listen address must be loopback.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"berthy/internal/cloudflare"
	"berthy/internal/cloudflare/cftest"
)

const maxControlBody = 1 << 20

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run starts the fake and serves until interrupted. It returns the process exit code: 2 for invalid flags, 1 for a
// failure to start or serve.
func run(args []string, logw io.Writer) int {
	fs := flag.NewFlagSet("fakecloudflare", flag.ContinueOnError)
	fs.SetOutput(logw)
	listen := fs.String("listen", "127.0.0.1:18787", "loopback address to serve on")
	account := fs.String("account", "", "Cloudflare account id the fake belongs to")
	tokenFile := fs.String("token-file", "", "file holding the API token; the token is never printed")
	zones := fs.String("zones", "", "comma-separated zone names the account holds")
	statePath := fs.String("state", "", "optional path for a JSON dump of the account, rewritten after every change")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(logw, "unexpected arguments")
		return 2
	}
	if err := checkLoopback(*listen); err != nil {
		fmt.Fprintln(logw, err)
		return 2
	}
	if *account == "" || *tokenFile == "" || *zones == "" {
		fmt.Fprintln(logw, "-account, -token-file and -zones are required")
		return 2
	}
	token, err := readToken(*tokenFile)
	if err != nil {
		fmt.Fprintln(logw, err)
		return 2
	}
	f := cftest.New(token, *account)
	for _, z := range strings.Split(*zones, ",") {
		if z = strings.TrimSpace(z); z != "" {
			f.AddZone(z)
		}
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(logw, "listen failed:", err)
		return 1
	}
	if *statePath != "" {
		if err := dumpState(f, *statePath); err != nil {
			fmt.Fprintln(logw, "state dump failed:", err)
			return 1
		}
	}
	srv := &http.Server{Handler: newMux(f, *statePath, logw), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	fmt.Fprintf(logw, "fakecloudflare serving %s\n", ln.Addr())
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(logw, "serve failed:", err)
		return 1
	}
	return 0
}

// checkLoopback refuses any listen address that is not a loopback address, because the control plane has no
// authentication.
func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid -listen address: %w", err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return errors.New("-listen must be a loopback address: the control plane has no authentication")
	}
	return nil
}

// readToken returns the API token in the file. The caller never prints it.
func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("cannot read -token-file: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", errors.New("-token-file is empty")
	}
	return token, nil
}

// newMux routes the Cloudflare API under /client/v4 and the control plane under /_fake/. When statePath is set, the state
// dump is rewritten before the response of every change is sent, so a client that has its answer can read the dump.
func newMux(f *cftest.Fake, statePath string, logw io.Writer) http.Handler {
	routes := http.NewServeMux()
	routes.Handle("/client/v4/", http.StripPrefix("/client/v4", f.Handler()))
	routes.Handle("/_fake/", controlPlane(f))
	var dumps sync.Mutex
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, r)
		if statePath != "" && r.Method != http.MethodGet && r.Method != http.MethodHead {
			dumps.Lock()
			err := dumpState(f, statePath)
			dumps.Unlock()
			if err != nil {
				fmt.Fprintln(logw, "state dump failed:", err)
			}
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	})
}

// controlPlane seeds and inspects the account. Every request is decoded strictly, so a misspelt field fails the test
// that sends it.
func controlPlane(f *cftest.Fake) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_fake/state", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, f.Snapshot())
	})
	mux.HandleFunc("POST /_fake/tunnels", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name   string          `json:"name"`
			Remote bool            `json:"remote"`
			Config json.RawMessage `json:"config"`
		}
		if !decodeControl(w, r, &body) {
			return
		}
		if body.Name == "" {
			writeError(w, http.StatusBadRequest, "name is required")
			return
		}
		id := f.AddTunnel(body.Name, body.Remote)
		if cfg := body.Config; len(cfg) > 0 && string(cfg) != "null" {
			f.SetConfig(id, cfg)
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
	})
	mux.HandleFunc("POST /_fake/records", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Zone   string               `json:"zone"`
			Record cloudflare.DNSRecord `json:"record"`
		}
		if !decodeControl(w, r, &body) {
			return
		}
		zoneID, ok := f.ZoneID(body.Zone)
		if !ok {
			writeError(w, http.StatusNotFound, "unknown zone")
			return
		}
		if body.Record.Type == "" || body.Record.Name == "" || body.Record.Content == "" {
			writeError(w, http.StatusBadRequest, "record type, name and content are required")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": f.AddRecord(zoneID, body.Record)})
	})
	mux.HandleFunc("POST /_fake/config", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tunnel string          `json:"tunnel"`
			Config json.RawMessage `json:"config"`
		}
		if !decodeControl(w, r, &body) {
			return
		}
		if !f.HasTunnel(body.Tunnel) {
			writeError(w, http.StatusNotFound, "unknown tunnel")
			return
		}
		cfg := body.Config
		if string(cfg) == "null" {
			cfg = nil
		}
		f.SetConfig(body.Tunnel, cfg)
		writeJSON(w, http.StatusOK, map[string]string{"tunnel": body.Tunnel})
	})
	mux.HandleFunc("POST /_fake/status", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tunnel      string `json:"tunnel"`
			Status      string `json:"status"`
			Connections int    `json:"connections"`
		}
		if !decodeControl(w, r, &body) {
			return
		}
		if !f.HasTunnel(body.Tunnel) {
			writeError(w, http.StatusNotFound, "unknown tunnel")
			return
		}
		switch body.Status {
		case "healthy", "degraded", "down", "inactive":
		default:
			writeError(w, http.StatusBadRequest, "status must be healthy, degraded, down or inactive")
			return
		}
		if body.Connections < 0 || body.Connections > 10 {
			writeError(w, http.StatusBadRequest, "connections must be between 0 and 10")
			return
		}
		f.SetTunnelStatus(body.Tunnel, body.Status, body.Connections)
		writeJSON(w, http.StatusOK, map[string]string{"status": body.Status})
	})
	mux.HandleFunc("POST /_fake/fail", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Method string `json:"method"`
			Prefix string `json:"prefix"`
			Status int    `json:"status"`
		}
		if !decodeControl(w, r, &body) {
			return
		}
		if body.Method == "" || !strings.HasPrefix(body.Prefix, "/") || body.Status < 400 || body.Status > 599 {
			writeError(w, http.StatusBadRequest, "method, a path prefix and a 4xx or 5xx status are required")
			return
		}
		f.FailRequests(body.Method, body.Prefix, body.Status)
		writeJSON(w, http.StatusOK, body)
	})
	mux.HandleFunc("DELETE /_fake/fail", func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxControlBody))
		if err != nil {
			writeError(w, http.StatusBadRequest, "cannot read request body")
			return
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			f.ClearFailures()
			writeJSON(w, http.StatusOK, map[string]string{"cleared": "all"})
			return
		}
		var body struct {
			Method string `json:"method"`
			Prefix string `json:"prefix"`
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if dec.Decode(&body) != nil || body.Method == "" || body.Prefix == "" {
			writeError(w, http.StatusBadRequest, "method and prefix are required to clear one fault")
			return
		}
		f.FailRequests(body.Method, body.Prefix, 0)
		writeJSON(w, http.StatusOK, map[string]string{"cleared": body.Method + " " + body.Prefix})
	})
	return mux
}

// decodeControl reads one strict JSON object from a control request, and answers 400 when it is not valid.
func decodeControl(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxControlBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// dumpState replaces the file at path with the account's state, written through a temporary file in the same directory.
func dumpState(f *cftest.Fake, path string) error {
	b, err := json.MarshalIndent(f.Snapshot(), "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(append(b, '\n'))
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}
