// Command faketelegram serves the part of the Telegram Bot API that alerts use, on a loopback address, so alerts can be
// exercised end to end without Telegram. It is a test tool. The agent and the CLI never import it, so it is not part of
// the production binaries.
//
// The Bot API is served under /bot<token>/, which is the agent's telegram_api_url plus that path. Each sendMessage that
// the fake accepts is recorded in a JSON file, and a control plane under /_fake/ reads and steers it: /_fake/messages
// lists what was sent, /_fake/fail makes sendMessage answer an HTTP status instead, and /_fake/reset clears the record.
// The control plane has no authentication, so the listen address must be loopback. The bot token is never recorded,
// printed or put in a log line; the path a request arrived on is never logged either.
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

// maxBody bounds the form a client may send.
const maxBody = 64 << 10

// message is one sendMessage the fake accepted. The token is not part of it.
type message struct {
	Time   time.Time `json:"time"`
	ChatID string    `json:"chat_id"`
	Text   string    `json:"text"`
}

// fake is the state of the Bot API: the token it accepts, what it has recorded, and whether it is failing.
type fake struct {
	token     string // the bot token sendMessage requires; empty accepts any token
	statePath string // optional JSON file rewritten after every recorded change
	logw      io.Writer
	now       func() time.Time

	mu       sync.Mutex
	messages []message
	failWith int // when non-zero, sendMessage answers this HTTP status and records nothing
	nextID   int
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run starts the fake and serves until interrupted. It returns the process exit code: 2 for invalid flags, 1 for a failure
// to start or serve.
func run(args []string, logw io.Writer) int {
	fs := flag.NewFlagSet("faketelegram", flag.ContinueOnError)
	fs.SetOutput(logw)
	listen := fs.String("listen", "127.0.0.1:18788", "loopback address to serve on")
	tokenFile := fs.String("token-file", "", "optional file holding the bot token sendMessage requires; the token is never printed")
	statePath := fs.String("messages", "", "optional path for a JSON dump of the recorded messages, rewritten after every change")
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
	token := ""
	if *tokenFile != "" {
		t, err := readToken(*tokenFile)
		if err != nil {
			fmt.Fprintln(logw, err)
			return 2
		}
		token = t
	}
	f := &fake{token: token, statePath: *statePath, logw: logw, now: time.Now}
	if *statePath != "" {
		if err := f.dump(); err != nil {
			fmt.Fprintln(logw, "message dump failed:", err)
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
	fmt.Fprintf(logw, "faketelegram serving %s\n", ln.Addr())
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

// readToken returns the bot token in the file. The caller never prints it.
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

// ServeHTTP routes the Bot API under /bot<token>/ and the control plane under /_fake/.
func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/_fake/") {
		f.control().ServeHTTP(w, r)
		return
	}
	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/bot") && strings.HasSuffix(r.URL.Path, "/sendMessage") {
		token := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/bot"), "/sendMessage")
		f.sendMessage(w, r, token)
		return
	}
	writeAPIError(w, http.StatusNotFound, "Not Found")
}

// control routes the control plane.
func (f *fake) control() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_fake/messages", f.listMessages)
	mux.HandleFunc("POST /_fake/fail", f.setFailure)
	mux.HandleFunc("POST /_fake/reset", f.reset)
	return mux
}

// sendMessage accepts a message when the token in the path matches, records it, and answers like the Bot API. A fake told
// to fail answers that status and records nothing.
func (f *fake) sendMessage(w http.ResponseWriter, r *http.Request, token string) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if f.token != "" && token != f.token {
		writeAPIError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeAPIError(w, http.StatusBadRequest, "Bad Request: cannot parse form")
		return
	}
	chat, text := r.PostForm.Get("chat_id"), r.PostForm.Get("text")
	if chat == "" || text == "" {
		writeAPIError(w, http.StatusBadRequest, "Bad Request: chat_id and text are required")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != 0 {
		writeAPIError(w, f.failWith, "injected failure")
		return
	}
	f.nextID++
	f.messages = append(f.messages, message{Time: f.now().UTC(), ChatID: chat, Text: text})
	if err := f.dumpLocked(); err != nil {
		fmt.Fprintln(f.logw, "message dump failed:", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": f.nextID, "text": text}})
}

// listMessages returns every recorded message, oldest first.
func (f *fake) listMessages(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]message{}, f.messages...)
	writeJSON(w, http.StatusOK, out)
}

// setFailure makes sendMessage answer the given HTTP status, or resumes normal answers when status is 0.
func (f *fake) setFailure(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Status int `json:"status"`
	}
	if !decodeControl(w, r, &body) {
		return
	}
	if body.Status != 0 && (body.Status < 400 || body.Status > 599) {
		writeControlError(w, http.StatusBadRequest, "status must be 0 or an HTTP error status")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failWith = body.Status
	writeJSON(w, http.StatusOK, map[string]int{"status": f.failWith})
}

// reset clears the record and resumes normal answers.
func (f *fake) reset(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = nil
	f.failWith = 0
	f.nextID = 0
	if err := f.dumpLocked(); err != nil {
		fmt.Fprintln(f.logw, "message dump failed:", err)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// dump writes the record to the state file, if one is configured.
func (f *fake) dump() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dumpLocked()
}

// dumpLocked writes the record to the state file through a temporary file, so a reader never sees half of it. The caller
// holds f.mu.
func (f *fake) dumpLocked() error {
	if f.statePath == "" {
		return nil
	}
	b, err := json.MarshalIndent(append([]message{}, f.messages...), "", "  ")
	if err != nil {
		return err
	}
	tmp := f.statePath + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, f.statePath)
}

// decodeControl reads one strict JSON object for a control request. It writes the error and returns false when the body
// is not exactly that.
func decodeControl(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		writeControlError(w, http.StatusBadRequest, "invalid control request")
		return false
	}
	return true
}

// writeAPIError answers in the Bot API's error form.
func writeAPIError(w http.ResponseWriter, status int, description string) {
	writeJSON(w, status, map[string]any{"ok": false, "error_code": status, "description": description})
}

func writeControlError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
