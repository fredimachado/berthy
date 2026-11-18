package monitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// testToken is a bot token of the documented shape, made up for these tests.
const testToken = "123456789:AAFake-token_for_tests"

func TestTelegramSendsChatAndText(t *testing.T) {
	var mu sync.Mutex
	var path string
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		path = r.URL.Path
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		form = r.PostForm
		w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer srv.Close()
	tg := &Telegram{BaseURL: srv.URL, Token: testToken, ChatID: "-1001234567890"}
	if err := tg.Notify(context.Background(), Alert{Text: "🔴 lab probe:site: HTTP 503"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if path != "/bot"+testToken+"/sendMessage" {
		t.Fatalf("path %q", path)
	}
	if form.Get("chat_id") != "-1001234567890" || form.Get("text") != "🔴 lab probe:site: HTTP 503" || form.Get("disable_web_page_preview") != "true" {
		t.Fatalf("form %v", form)
	}
}

// recordTransport answers every request with a fixed reply and remembers the last request.
type recordTransport struct {
	mu    sync.Mutex
	last  *http.Request
	reply string
	err   error
}

func (rt *recordTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.last = r
	if rt.err != nil {
		return nil, rt.err
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(rt.reply)), Header: make(http.Header), Request: r}, nil
}

func TestTelegramDefaultsToTheBotAPI(t *testing.T) {
	rt := &recordTransport{reply: `{"ok":true}`}
	tg := &Telegram{Token: testToken, ChatID: "@berthy_alerts", HTTP: &http.Client{Transport: rt}}
	if err := tg.Notify(context.Background(), Alert{Text: "test"}); err != nil {
		t.Fatal(err)
	}
	if got := rt.last.URL.String(); got != "https://api.telegram.org/bot"+testToken+"/sendMessage" {
		t.Fatalf("default URL %q", got)
	}
}

func TestTelegramAPIErrorsCarryOnlyTheStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
	}))
	defer srv.Close()
	err := (&Telegram{BaseURL: srv.URL, Token: testToken, ChatID: "1"}).Notify(context.Background(), Alert{Text: "x"})
	var status *TelegramStatusError
	if !errors.As(err, &status) || status.Code != http.StatusUnauthorized {
		t.Fatalf("status error: %v", err)
	}
	if err.Error() != "telegram returned HTTP 401" || strings.Contains(err.Error(), testToken) {
		t.Fatalf("error text: %q", err.Error())
	}
}

func TestTelegramTransportErrorsNeverShowTheToken(t *testing.T) {
	// The HTTP client wraps transport errors with the request URL, which contains the token.
	rt := &recordTransport{err: &url.Error{Op: "Post", URL: "https://api.telegram.org/bot" + testToken + "/sendMessage", Err: errors.New("connection reset")}}
	err := (&Telegram{Token: testToken, ChatID: "1", HTTP: &http.Client{Transport: rt}}).Notify(context.Background(), Alert{Text: "x"})
	if !errors.Is(err, ErrTelegramUnavailable) || err.Error() != "telegram unavailable" {
		t.Fatalf("transport error: %v", err)
	}
	if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), "bot") {
		t.Fatalf("error text leaks the request: %q", err.Error())
	}

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()
	err = (&Telegram{BaseURL: srv.URL, Token: testToken, ChatID: "1"}).Notify(context.Background(), Alert{Text: "x"})
	if !errors.Is(err, ErrTelegramUnavailable) || strings.Contains(err.Error(), testToken) {
		t.Fatalf("closed server: %v", err)
	}

	err = (&Telegram{BaseURL: "http://[::1", Token: testToken, ChatID: "1"}).Notify(context.Background(), Alert{Text: "x"})
	if !errors.Is(err, ErrTelegramUnavailable) || strings.Contains(err.Error(), testToken) {
		t.Fatalf("invalid base URL: %v", err)
	}
}

func TestTelegramRejectsUnexpectedAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>not the Bot API</html>`))
	}))
	defer srv.Close()
	err := (&Telegram{BaseURL: srv.URL, Token: testToken, ChatID: "1"}).Notify(context.Background(), Alert{Text: "x"})
	if !errors.Is(err, ErrTelegramUnavailable) {
		t.Fatalf("non-API answer accepted: %v", err)
	}
}

func TestTelegramRefusesRedirects(t *testing.T) {
	var hits int
	var mu sync.Mutex
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	err := (&Telegram{BaseURL: redirect.URL, Token: testToken, ChatID: "1"}).Notify(context.Background(), Alert{Text: "x"})
	if err == nil {
		t.Fatal("redirect followed to a second host")
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatal("redirect target received the token")
	}
}

func TestTelegramFormattingHidesTheToken(t *testing.T) {
	tg := &Telegram{BaseURL: "https://api.telegram.org", Token: testToken, ChatID: "-100"}
	for _, text := range []string{fmt.Sprintf("%v", tg), fmt.Sprintf("%+v", tg), fmt.Sprintf("%#v", tg), fmt.Sprintf("%v", *tg)} {
		if strings.Contains(text, testToken) {
			t.Fatalf("formatted client leaks the token: %q", text)
		}
	}
}
