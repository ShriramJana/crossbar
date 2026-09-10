// Package provider defines the gateway's view of an upstream LLM backend and
// the adapters that implement it.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Provider is the only thing the router knows about a backend.
type Provider interface {
	// Name identifies the provider in logs, metrics, and response headers.
	Name() string
	// Send forwards one normalized request and returns the normalized response.
	// Failures are returned as *Error so callers can classify them.
	Send(ctx context.Context, req *Request) (*Response, error)
	// HealthCheck issues the cheapest possible upstream call to confirm liveness.
	HealthCheck(ctx context.Context) error
}

// Message is a single turn in a conversation.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request is the provider-agnostic request shape.
type Request struct {
	Model       string
	Messages    []Message
	MaxTokens   int
	Temperature float64
	System      string
}

// Response is the provider-agnostic response shape.
type Response struct {
	Text         string
	InputTokens  int
	OutputTokens int
	// Model is the model that actually served the request.
	Model string
	// Provider is the provider that actually served the request.
	Provider string
	Latency  time.Duration
	CostUSD  float64
	// Fallback is true if the primary provider was skipped.
	Fallback bool
}

// ErrorClass groups upstream failures by how the gateway should react.
type ErrorClass int

const (
	// ErrRetryable covers 429, 5xx, and timeouts: try again, count against the breaker.
	ErrRetryable ErrorClass = iota
	// ErrNonRetryable covers 4xx client faults and policy refusals: return to caller as-is.
	ErrNonRetryable
	// ErrTransport covers dial, DNS, and connection-reset failures: retry, count against the breaker.
	ErrTransport
)

// String returns a stable label for metrics and logs.
func (c ErrorClass) String() string {
	switch c {
	case ErrRetryable:
		return "retryable"
	case ErrNonRetryable:
		return "non_retryable"
	case ErrTransport:
		return "transport"
	default:
		return fmt.Sprintf("unknown(%d)", int(c))
	}
}

// Error wraps an upstream failure with its class, provider, and HTTP status.
// StatusCode is 0 when no HTTP response was received.
type Error struct {
	Provider   string
	Class      ErrorClass
	StatusCode int
	Err        error
}

// Error implements error.
func (e *Error) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("%s: %s (status %d): %v", e.Provider, e.Class, e.StatusCode, e.Err)
	}
	return fmt.Sprintf("%s: %s: %v", e.Provider, e.Class, e.Err)
}

// Unwrap exposes the underlying cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.Err }

// ClassOf extracts the ErrorClass from err if it wraps an *Error.
func ClassOf(err error) (ErrorClass, bool) {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Class, true
	}
	return 0, false
}

// ClassifyStatus maps an upstream HTTP status to an ErrorClass.
//
// Anything 5xx (including non-standard codes such as Anthropic's 529
// "overloaded") is retryable. Of the 4xx family only 408 and 429 are
// transient; every other 4xx reflects a problem with the request itself.
func ClassifyStatus(status int) ErrorClass {
	switch {
	case status >= 500:
		return ErrRetryable
	case status == http.StatusRequestTimeout, status == http.StatusTooManyRequests:
		return ErrRetryable
	default:
		return ErrNonRetryable
	}
}

// classifyErr maps a non-HTTP failure from a transport call to an ErrorClass.
// Context expiry is treated like an upstream timeout (retryable); everything
// else that reaches here is a network-level fault.
func classifyErr(err error) ErrorClass {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ErrRetryable
	}
	return ErrTransport
}
