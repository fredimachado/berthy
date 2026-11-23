// Package credtest is an in-memory credential store for tests. It imports no testing package, so an end-to-end
// binary can use it too.
package credtest

import (
	"sync"

	"berthy/internal/credstore"
)

// Memory is a Store in memory for tests.
type Memory struct {
	SetErr, DeleteErr error
	Block             <-chan struct{} // when set, every call waits until it is closed
	OnCall            func()          // when set, called as each call starts

	mu       sync.Mutex
	cond     *sync.Cond
	vals     map[credstore.Key]string
	calls    int
	inflight int
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	m := &Memory{vals: map[credstore.Key]string{}}
	m.cond = sync.NewCond(&m.mu)
	return m
}

// Name returns "test store".
func (m *Memory) Name() string { return "test store" }

// Len returns how many tokens are stored.
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.vals)
}

// Calls returns how many times Get, Set or Delete was called.
func (m *Memory) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// Wait returns when no call is running: a test waits for a released call to finish.
func (m *Memory) Wait() {
	m.mu.Lock()
	for m.inflight > 0 {
		m.cond.Wait()
	}
	m.mu.Unlock()
}

func (m *Memory) begin() {
	m.mu.Lock()
	m.calls++
	m.inflight++
	onCall := m.OnCall
	block := m.Block
	m.mu.Unlock()
	if onCall != nil {
		onCall()
	}
	if block != nil {
		<-block
	}
}

func (m *Memory) end() {
	m.mu.Lock()
	m.inflight--
	if m.inflight == 0 {
		m.cond.Broadcast()
	}
	m.mu.Unlock()
}

// Get returns a stored token, or credstore.ErrNotFound.
func (m *Memory) Get(k credstore.Key) (string, error) {
	m.begin()
	defer m.end()
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vals[k]
	if !ok {
		return "", credstore.ErrNotFound
	}
	return v, nil
}

// Set stores a token.
func (m *Memory) Set(k credstore.Key, secret string) error {
	m.begin()
	defer m.end()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.SetErr != nil {
		return m.SetErr
	}
	m.vals[k] = secret
	return nil
}

// Delete removes a token. It returns (false, nil) when there was none.
func (m *Memory) Delete(k credstore.Key) (bool, error) {
	m.begin()
	defer m.end()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.DeleteErr != nil {
		return false, m.DeleteErr
	}
	if _, ok := m.vals[k]; !ok {
		return false, nil
	}
	delete(m.vals, k)
	return true, nil
}
