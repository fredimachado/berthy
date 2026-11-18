// Command fakewebhook is an HTTP endpoint for the agent's webhook alerts and heartbeat pings, on a loopback address, so both
// can be exercised end to end without a real service. It is a test tool. The agent and the CLI never import it, so it is not
// part of the production binaries.
//
// Every request outside /_fake/ is recorded: its method, path, headers and body. The record is appended as one JSON object
// per line to the file named by -requests, and a control plane under /_fake/ reads and steers it: /_fake/requests lists what
// arrived, /_fake/fail makes the endpoint answer an HTTP status instead of 204, and /_fake/reset clears the record. The
// control plane has no authentication, so the listen address must be loopback. The path of a request is part of the record,
// since the agent's webhook URL often carries a credential in it, but it is never printed or put in a log line.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// maxBody bounds the body a client may send.
const maxBody = 64 << 10

// request is one request the fake received.
type request struct {
	Time    time.Time           `json:"time"`
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
}

// fake is the state of the endpoint: what it has recorded, and whether it is failing.
type fake struct {
	recordPath string // optional file that every request is appended to, one JSON object per line
	logw       io.Writer
	now        func() time.Time

	mu       sync.Mutex
	requests []request
	failWith int // when non-zero, every request is recorded and answered with this HTTP status
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run starts the fake and serves until interrupted. It returns the process exit code: 2 for invalid flags, 1 for a failure
// to start or serve.
func run(args []string, logw io.Writer) int {
	fs := flag.NewFlagSet("fakewebhook", flag.ContinueOnError)
	fs.SetOutput(logw)
	listen := fs.String("listen", "127.0.0.1:18789", "loopback address to serve on")
	record := fs.String("requests", "", "optional path of a JSON Lines file that every request is appended to")
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
	f := &fake{recordPath: *record, logw: logw, now: time.Now}
	if *record != "" {
		if err := os.WriteFile(*record, nil, 0600); err != nil {
			fmt.Fprintln(logw, "cannot write -requests:", err)
			return 1
		}
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(logw, "listen failed:", err)
		return 1
	}
	srv := &http.Server{Handler: f, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	fmt.Fprintf(logw, "fakewebhook serving %s\n", ln.Addr())
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

// ServeHTTP routes the control plane under /_fake/ and records every other request.
func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/_fake/") {
		f.control().ServeHTTP(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request{Time: f.now().UTC(), Method: r.Method, Path: r.URL.Path, Headers: r.Header.Clone(), Body: string(body)})
	if err := f.appendLocked(f.requests[len(f.requests)-1]); err != nil {
		fmt.Fprintln(f.logw, "request record failed:", err)
	}
	if f.failWith != 0 {
		w.WriteHeader(f.failWith)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// control routes the control plane.
func (f *fake) control() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_fake/requests", f.list)
	mux.HandleFunc("POST /_fake/fail", f.setFailure)
	mux.HandleFunc("POST /_fake/reset", f.reset)
	return mux
}

// list returns every recorded request, oldest first.
func (f *fake) list(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	writeJSON(w, http.StatusOK, append([]request{}, f.requests...))
}

// setFailure makes the endpoint answer the given HTTP status, or resumes normal answers when status is 0.
func (f *fake) setFailure(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Status int `json:"status"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid control request"})
		return
	}
	if body.Status != 0 && (body.Status < 400 || body.Status > 599) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status must be 0 or an HTTP error status"})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failWith = body.Status
	writeJSON(w, http.StatusOK, map[string]int{"status": f.failWith})
}

// reset clears the record and resumes normal answers. The record file is emptied too.
func (f *fake) reset(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
	f.failWith = 0
	if f.recordPath != "" {
		if err := os.WriteFile(f.recordPath, nil, 0600); err != nil {
			fmt.Fprintln(f.logw, "request record failed:", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// appendLocked adds one request to the record file, if one is configured. The caller holds f.mu.
func (f *fake) appendLocked(r request) error {
	if f.recordPath == "" {
		return nil
	}
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(f.recordPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(append(line, '\n'))
	return err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
