package monitor

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Alert reports a check or the end of a deploy. Text is the line Telegram sends, ready to show. No field holds application
// output or a secret value.
type Alert struct {
	Environment string
	Server      string
	Kind        string // "check" or "deploy"
	Name        string // the check, or the app of a deploy
	State       string // the check's state, or the end state of the deploy
	Detail      string
	Since       time.Time // when the state began; zero when it is not known
	Text        string
}

// Subject names the server or environment the alert concerns.
func (a Alert) Subject() string {
	if a.Server != "" {
		return "server " + a.Server
	}
	return a.Environment
}

// Notifier delivers an alert to one channel.
type Notifier interface {
	Notify(ctx context.Context, a Alert) error
}

// channeler is implemented by a notifier that can name its channel.
type channeler interface{ Channel() string }

// ChannelName returns the name of the notifier's channel, for a message or a log line.
func ChannelName(n Notifier) string {
	if c, ok := n.(channeler); ok {
		return c.Channel()
	}
	return "alert channel"
}

// Notifiers delivers an alert to every channel at the same time, so a slow channel does not hold the others back.
type Notifiers []Notifier

// DeliveryError means some channels refused an alert and at least one took it, so the alert reached the people anyway.
type DeliveryError struct {
	Err       error // the failed channels, each named
	Delivered int   // how many channels took the alert
}

func (e *DeliveryError) Error() string { return e.Err.Error() }
func (e *DeliveryError) Unwrap() error { return e.Err }

// Notify sends a to every channel. It returns nil when each took it, a *DeliveryError when only some did, and the plain error
// of the failed channels when none did. Each failure is named by its channel and never holds a URL or a token.
func (n Notifiers) Notify(ctx context.Context, a Alert) error {
	errs := make([]error, len(n))
	var wg sync.WaitGroup
	for i, c := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Notify(ctx, a); err != nil {
				errs[i] = &channelError{channel: ChannelName(c), err: err}
			}
		}()
	}
	wg.Wait()
	failed := errors.Join(errs...)
	if failed == nil {
		return nil
	}
	delivered := 0
	for _, err := range errs {
		if err == nil {
			delivered++
		}
	}
	if delivered == 0 {
		return failed
	}
	return &DeliveryError{Err: failed, Delivered: delivered}
}

// channelError names the channel of an error.
type channelError struct {
	channel string
	err     error
}

func (e *channelError) Error() string { return e.channel + ": " + e.err.Error() }
func (e *channelError) Unwrap() error { return e.err }
