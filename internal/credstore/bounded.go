package credstore

import (
	"context"
	"time"
)

// Bounded returns s with every call given up when ctx ends or after wait, whichever comes first. A call that is given up
// answers ErrGivenUp, and what it returns later is dropped. The call keeps running in its goroutine; a CLI that exits
// while it runs simply ends it, which is accepted. When ctx is already done, the store is not called.
func Bounded(ctx context.Context, s Store, wait time.Duration) Store {
	return &bounded{ctx: ctx, s: s, wait: wait}
}

type bounded struct {
	ctx  context.Context
	s    Store
	wait time.Duration
}

func (b *bounded) Name() string { return b.s.Name() }

func (b *bounded) Get(k Key) (string, error) {
	if b.ctx.Err() != nil {
		return "", ErrGivenUp
	}
	type result struct {
		v   string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		v, err := b.s.Get(k)
		ch <- result{v, err}
	}()
	timer := time.NewTimer(b.wait)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-b.ctx.Done():
		return "", ErrGivenUp
	case <-timer.C:
		return "", ErrGivenUp
	}
}

func (b *bounded) Set(k Key, secret string) error {
	if b.ctx.Err() != nil {
		return ErrGivenUp
	}
	ch := make(chan error, 1)
	go func() { ch <- b.s.Set(k, secret) }()
	timer := time.NewTimer(b.wait)
	defer timer.Stop()
	select {
	case err := <-ch:
		return err
	case <-b.ctx.Done():
		return ErrGivenUp
	case <-timer.C:
		return ErrGivenUp
	}
}

func (b *bounded) Delete(k Key) (bool, error) {
	if b.ctx.Err() != nil {
		return false, ErrGivenUp
	}
	type result struct {
		removed bool
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		removed, err := b.s.Delete(k)
		ch <- result{removed, err}
	}()
	timer := time.NewTimer(b.wait)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.removed, r.err
	case <-b.ctx.Done():
		return false, ErrGivenUp
	case <-timer.C:
		return false, ErrGivenUp
	}
}
