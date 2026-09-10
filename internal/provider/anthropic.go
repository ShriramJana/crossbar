package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	anthropicDefaultBaseURL = "https://api.anthropic.com"
	anthropicVersion        = "2023-06-01"
	// maxErrorBody bounds how much of an upstream error body is read into memory.
	maxErrorBody = 64 << 10
)

// AnthropicConfig configures the Anthropic Messages API adapter.
type AnthropicConfig struct {
	// BaseURL overrides the API host. Defaults to https://api.anthropic.com.
	BaseURL string
	// APIKey is sent as the x-api-key header.
	APIKey string
	// Client is the HTTP client to use. Defaults to http.DefaultClient.
	Client *http.Client
}

// Anthropic speaks the Anthropic Messages API over plain HTTP.
type Anthropic struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

// NewAnthropic builds an Anthropic adapter.
func NewAnthropic(cfg AnthropicConfig) *Anthropic {
	if cfg.BaseURL == "" {
		cfg.BaseURL = anthropicDefaultBaseURL
	}
	if cfg.Client == nil {
		cfg.Client = http.DefaultClient
	}
	return &Anthropic{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:  cfg.APIKey,
		client:  cfg.Client,
	}
}

// Name implements Provider.
func (a *Anthropic) Name() string { return "anthropic" }

// Wire types for the Messages API. Only the fields the gateway uses are modelled.

type anthropicRequest struct {
	Model       string    `json:"model"`
	MaxTokens   int       `json:"max_tokens"`
	Messages    []Message `json:"messages"`
	System      string    `json:"system,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
}

type anthropicResponse struct {
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type anthropicErrorBody struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Send implements Provider.
func (a *Anthropic) Send(ctx context.Context, req *Request) (*Response, error) {
	wire := anthropicRequest{
		Model:     req.Model,
		MaxTokens: req.MaxTokens,
		Messages:  req.Messages,
		System:    req.System,
	}
	if req.Temperature != 0 {
		t := req.Temperature
		wire.Temperature = &t
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, &Error{Provider: a.Name(), Class: ErrNonRetryable, Err: fmt.Errorf("encoding request: %w", err)}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, &Error{Provider: a.Name(), Class: ErrNonRetryable, Err: fmt.Errorf("building request: %w", err)}
	}
	a.setHeaders(httpReq)
	httpReq.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, &Error{Provider: a.Name(), Class: classifyErr(err), Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, a.errorFromResponse(resp)
	}

	var out anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		// A 2xx with an unreadable body is an upstream fault, not the caller's.
		return nil, &Error{Provider: a.Name(), Class: ErrRetryable, StatusCode: resp.StatusCode, Err: fmt.Errorf("decoding response: %w", err)}
	}

	var text strings.Builder
	for _, block := range out.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	return &Response{
		Text:         text.String(),
		InputTokens:  out.Usage.InputTokens,
		OutputTokens: out.Usage.OutputTokens,
		Model:        out.Model,
		Provider:     a.Name(),
		Latency:      time.Since(start),
	}, nil
}

// HealthCheck implements Provider. It lists models rather than sending a
// message so the 30-second prober never incurs token charges.
func (a *Anthropic) HealthCheck(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/v1/models?limit=1", nil)
	if err != nil {
		return &Error{Provider: a.Name(), Class: ErrNonRetryable, Err: fmt.Errorf("building request: %w", err)}
	}
	a.setHeaders(httpReq)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return &Error{Provider: a.Name(), Class: classifyErr(err), Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &Error{
			Provider:   a.Name(),
			Class:      ClassifyStatus(resp.StatusCode),
			StatusCode: resp.StatusCode,
			Err:        errors.New(http.StatusText(resp.StatusCode)),
		}
	}
	return nil
}

func (a *Anthropic) setHeaders(r *http.Request) {
	r.Header.Set("x-api-key", a.apiKey)
	r.Header.Set("anthropic-version", anthropicVersion)
	r.Header.Set("Accept", "application/json")
}

// errorFromResponse builds an *Error from a non-2xx response, surfacing the
// upstream message when the body is the documented JSON error shape.
func (a *Anthropic) errorFromResponse(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))

	var body anthropicErrorBody
	msg := strings.TrimSpace(string(raw))
	if err := json.Unmarshal(raw, &body); err == nil && body.Error.Message != "" {
		msg = body.Error.Type + ": " + body.Error.Message
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return &Error{
		Provider:   a.Name(),
		Class:      ClassifyStatus(resp.StatusCode),
		StatusCode: resp.StatusCode,
		Err:        errors.New(msg),
	}
}
