package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Behavior describes one scripted outcome of a Mock call.
type Behavior struct {
	// Status, when non-zero, makes the call fail with that HTTP status and the
	// class ClassifyStatus assigns it.
	Status int
	// Transport, when true, makes the call fail as a network-level error.
	Transport bool
	// Timeout, when true, blocks until ctx expires and then fails as retryable.
	Timeout bool
	// Delay is waited (honouring ctx) before the outcome is produced.
	Delay time.Duration
	// Text is the response body on success. Empty means a generated echo.
	Text string
}

// Succeed returns a behavior that responds with the given text.
func Succeed(text string) Behavior { return Behavior{Text: text} }

// Fail returns a behavior that fails with the given HTTP status.
func Fail(status int) Behavior { return Behavior{Status: status} }

// Transport returns a behavior that fails as a connection-level error.
func Transport() Behavior { return Behavior{Transport: true} }

// Timeout returns a behavior that blocks until the caller's context expires.
func Timeout() Behavior { return Behavior{Timeout: true} }

// Slow returns a behavior that succeeds after d, or fails if ctx expires first.
func Slow(d time.Duration) Behavior { return Behavior{Delay: d} }

// Mock is a scriptable in-memory Provider. Every resilience test runs against
// it: queued behaviors are consumed in FIFO order, after which the default
// behavior applies. It is safe for concurrent use.
type Mock struct {
	name string

	mu      sync.Mutex
	queue   []Behavior
	def     Behavior
	calls   int
	lastReq *Request
}

// NewMock returns a Mock that succeeds by default.
func NewMock(name string) *Mock {
	return &Mock{name: name}
}

// Name implements Provider.
func (m *Mock) Name() string { return m.name }

// Enqueue appends behaviors to be consumed by subsequent calls, in order.
func (m *Mock) Enqueue(bs ...Behavior) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queue = append(m.queue, bs...)
}

// SetDefault sets the behavior used once the queue is empty.
func (m *Mock) SetDefault(b Behavior) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.def = b
}

// Calls returns how many Send calls have been made.
func (m *Mock) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// LastRequest returns the most recent request passed to Send, or nil.
func (m *Mock) LastRequest() *Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastReq
}

func (m *Mock) next(req *Request) Behavior {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	m.lastReq = req
	if len(m.queue) == 0 {
		return m.def
	}
	b := m.queue[0]
	m.queue = m.queue[1:]
	return b
}

// Send implements Provider.
func (m *Mock) Send(ctx context.Context, req *Request) (*Response, error) {
	start := time.Now()
	b := m.next(req)

	if err := m.run(ctx, b); err != nil {
		return nil, err
	}

	text := b.Text
	if text == "" {
		text = "mock response to: " + lastUserContent(req)
	}
	return &Response{
		Text:         text,
		InputTokens:  approxTokens(req),
		OutputTokens: max(1, len(strings.Fields(text))),
		Model:        req.Model,
		Provider:     m.name,
		Latency:      time.Since(start),
	}, nil
}

// HealthCheck implements Provider. It follows the default behavior only, so
// queued failures scripted for Send are not consumed by the prober.
func (m *Mock) HealthCheck(ctx context.Context) error {
	m.mu.Lock()
	b := m.def
	m.mu.Unlock()
	return m.run(ctx, b)
}

// run produces the failure (if any) described by b, waiting first if asked.
func (m *Mock) run(ctx context.Context, b Behavior) error {
	if b.Timeout {
		<-ctx.Done()
		return &Error{Provider: m.name, Class: ErrRetryable, Err: ctx.Err()}
	}
	if b.Delay > 0 {
		t := time.NewTimer(b.Delay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return &Error{Provider: m.name, Class: classifyErr(ctx.Err()), Err: ctx.Err()}
		}
	}
	switch {
	case b.Transport:
		return &Error{Provider: m.name, Class: ErrTransport, Err: errors.New("connection refused")}
	case b.Status != 0:
		return &Error{
			Provider:   m.name,
			Class:      ClassifyStatus(b.Status),
			StatusCode: b.Status,
			Err:        fmt.Errorf("scripted status %d", b.Status),
		}
	}
	return nil
}

func lastUserContent(req *Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return req.Messages[i].Content
		}
	}
	return ""
}

// approxTokens gives a deterministic, non-zero token count so cost accounting
// has something to work with in tests. It is a word count, not a tokenizer.
func approxTokens(req *Request) int {
	n := len(strings.Fields(req.System))
	for _, msg := range req.Messages {
		n += len(strings.Fields(msg.Content))
	}
	return max(1, n)
}
