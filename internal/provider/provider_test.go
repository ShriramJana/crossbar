package provider_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ShriramJana/crossbar/internal/provider"
)

func TestClassifyStatus(t *testing.T) {
	tests := []struct {
		status int
		want   provider.ErrorClass
	}{
		{http.StatusBadRequest, provider.ErrNonRetryable},
		{http.StatusUnauthorized, provider.ErrNonRetryable},
		{http.StatusForbidden, provider.ErrNonRetryable},
		{http.StatusNotFound, provider.ErrNonRetryable},
		{http.StatusRequestTimeout, provider.ErrRetryable},
		{http.StatusTooManyRequests, provider.ErrRetryable},
		{http.StatusInternalServerError, provider.ErrRetryable},
		{http.StatusBadGateway, provider.ErrRetryable},
		{http.StatusServiceUnavailable, provider.ErrRetryable},
		{http.StatusGatewayTimeout, provider.ErrRetryable},
		{529, provider.ErrRetryable}, // Anthropic "overloaded"
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.status), func(t *testing.T) {
			assert.Equal(t, tt.want, provider.ClassifyStatus(tt.status))
		})
	}
}

func TestProviderErrorInspection(t *testing.T) {
	cause := errors.New("upstream said no")
	err := fmt.Errorf("sending: %w", &provider.Error{
		Provider:   "anthropic",
		Class:      provider.ErrRetryable,
		StatusCode: 503,
		Err:        cause,
	})

	var pe *provider.Error
	require.True(t, errors.As(err, &pe), "errors.As must find the wrapped provider error")
	assert.Equal(t, "anthropic", pe.Provider)
	assert.Equal(t, provider.ErrRetryable, pe.Class)
	assert.Equal(t, 503, pe.StatusCode)
	assert.True(t, errors.Is(err, cause), "errors.Is must see through to the cause")
	assert.Contains(t, pe.Error(), "anthropic")
	assert.Contains(t, pe.Error(), "503")
}

func TestClassOf(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want provider.ErrorClass
		ok   bool
	}{
		{name: "nil", err: nil, ok: false},
		{name: "plain error", err: errors.New("x"), ok: false},
		{name: "wrapped provider error", err: fmt.Errorf("w: %w", &provider.Error{Class: provider.ErrTransport}), want: provider.ErrTransport, ok: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := provider.ClassOf(tt.err)
			assert.Equal(t, tt.ok, ok)
			if ok {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestErrorClassString(t *testing.T) {
	assert.Equal(t, "retryable", provider.ErrRetryable.String())
	assert.Equal(t, "non_retryable", provider.ErrNonRetryable.String())
	assert.Equal(t, "transport", provider.ErrTransport.String())
	assert.Equal(t, "unknown(7)", provider.ErrorClass(7).String())
}
