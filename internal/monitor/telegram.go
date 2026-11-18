package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultTelegramURL is the Telegram Bot API.
const DefaultTelegramURL = "https://api.telegram.org"

// ErrTelegramUnavailable means Telegram could not be reached, or did not answer as the Bot API does. Its text never
// includes the request URL, which contains the bot token.
var ErrTelegramUnavailable = errors.New("telegram unavailable")

// TelegramStatusError means Telegram answered with an HTTP status other than 200.
type TelegramStatusError struct{ Code int }

func (e *TelegramStatusError) Error() string {
	return fmt.Sprintf("telegram returned HTTP %d", e.Code)
}

// Telegram sends messages with the Telegram Bot API. The bot token is part of the request path, so this type never
// formats the token, and no error it returns contains the URL.
type Telegram struct {
	BaseURL string       // the API base URL; empty means DefaultTelegramURL
	Token   string       // the bot token, a secret
	ChatID  string       // the chat that receives the messages
	HTTP    *http.Client // nil means a client with a 10-second timeout that never follows redirects
}

// String hides the token, so a formatted Telegram cannot leak it into a log.
func (t Telegram) String() string { return "telegram(chat " + t.ChatID + ")" }

// GoString hides the token for %#v.
func (t Telegram) GoString() string { return t.String() }

// Channel names the channel in an error or a log line.
func (Telegram) Channel() string { return "telegram" }

// Notify sends the alert's text to the chat with the sendMessage method.
func (t *Telegram) Notify(ctx context.Context, a Alert) error {
	base := strings.TrimRight(t.BaseURL, "/")
	if base == "" {
		base = DefaultTelegramURL
	}
	form := url.Values{}
	form.Set("chat_id", t.ChatID)
	form.Set("text", a.Text)
	form.Set("disable_web_page_preview", "true")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+t.Token+"/sendMessage", strings.NewReader(form.Encode()))
	if err != nil {
		return ErrTelegramUnavailable
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := t.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		return ErrTelegramUnavailable
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return ErrTelegramUnavailable
	}
	if resp.StatusCode != http.StatusOK {
		return &TelegramStatusError{Code: resp.StatusCode}
	}
	var reply struct {
		OK bool `json:"ok"`
	}
	if json.Unmarshal(body, &reply) != nil || !reply.OK {
		return ErrTelegramUnavailable
	}
	return nil
}
