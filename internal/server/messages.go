package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ShriramJana/crossbar/internal/budget"
	"github.com/ShriramJana/crossbar/internal/config"
	"github.com/ShriramJana/crossbar/internal/limiter"
	"github.com/ShriramJana/crossbar/internal/provider"
	"github.com/ShriramJana/crossbar/internal/router"
)

// maxBodyBytes bounds a request body. Prompts are large; abuse is larger.
const maxBodyBytes = 4 << 20

// Anthropic-compatible wire shapes for POST /v1/messages.

type messagesRequest struct {
	Model       string        `json:"model"`
	MaxTokens   int           `json:"max_tokens"`
	System      string        `json:"system"`
	Temperature float64       `json:"temperature"`
	Messages    []wireMessage `json:"messages"`
}

type wireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type messagesResponse struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Model      string         `json:"model"`
	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      usage          `json:"usage"`
}

type usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// parseMessages decodes and validates the body into the provider-agnostic Request.
func parseMessages(r io.Reader) (*provider.Request, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var in messagesRequest
	if err := dec.Decode(&in); err != nil {
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("invalid_json: %w", err)
		}
		return nil, err
	}
	if in.Model == "" {
		return nil, errors.New("model is required")
	}
	if in.MaxTokens <= 0 {
		return nil, errors.New("max_tokens must be positive")
	}
	if len(in.Messages) == 0 {
		return nil, errors.New("messages must not be empty")
	}

	req := &provider.Request{
		Model:       in.Model,
		MaxTokens:   in.MaxTokens,
		System:      in.System,
		Temperature: in.Temperature,
		Messages:    make([]provider.Message, 0, len(in.Messages)),
	}
	for i, m := range in.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			return nil, fmt.Errorf("messages[%d]: role must be user or assistant", i)
		}
		text, err := flattenContent(m.Content)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		req.Messages = append(req.Messages, provider.Message{Role: m.Role, Content: text})
	}
	return req, nil
}

// flattenContent accepts either a string or an array of text blocks, the two
// content shapes the Anthropic API allows for text-only requests.
func flattenContent(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", errors.New("content must be a string or an array of text blocks")
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type != "text" {
			return "", fmt.Errorf("unsupported content block type %q", blk.Type)
		}
		b.WriteString(blk.Text)
	}
	return b.String(), nil
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	team, _ := TeamFromContext(r.Context())
	info := infoFromContext(r.Context())

	req, err := parseMessages(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !team.AllowsModel(req.Model) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "model_not_allowed", "model": req.Model})
		return
	}
	info.model = req.Model

	ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)
	defer cancel()

	if !s.admit(ctx, w, team, req) {
		return
	}

	resp, err := s.router.Dispatch(ctx, req)
	if err != nil {
		s.writeDispatchError(w, err)
		return
	}
	s.settle(r.Context(), team, req, resp)

	info.provider = resp.Provider
	info.model = resp.Model
	info.inputTokens = resp.InputTokens
	info.outputTokens = resp.OutputTokens
	info.costUSD = resp.CostUSD
	info.fallback = resp.Fallback

	h := w.Header()
	h.Set("X-Crossbar-Provider", resp.Provider)
	h.Set("X-Crossbar-Model", resp.Model)
	h.Set("X-Crossbar-Fallback", strconv.FormatBool(resp.Fallback))
	h.Set("X-Crossbar-Cost-USD", strconv.FormatFloat(resp.CostUSD, 'f', 6, 64))
	writeJSON(w, http.StatusOK, messagesResponse{
		ID:         "msg_" + RequestIDFromContext(r.Context()),
		Type:       "message",
		Role:       "assistant",
		Model:      resp.Model,
		Content:    []contentBlock{{Type: "text", Text: resp.Text}},
		StopReason: "end_turn",
		Usage:      usage{InputTokens: resp.InputTokens, OutputTokens: resp.OutputTokens},
	})
}

// admit runs the rate limit and budget checks, writing the rejection and
// returning false if either denies. A backend failure fails open: the request
// proceeds and the failure is logged, since refusing all traffic whenever the
// sidecar hiccups would turn the gateway into the outage it exists to prevent.
func (s *Server) admit(ctx context.Context, w http.ResponseWriter, team *config.Team, req *provider.Request) bool {
	if s.limiter != nil {
		lim := limiter.Limits{RequestsPerMinute: team.RequestsPerMinute, TokensPerMinute: team.TokensPerMinute}
		d, err := s.limiter.Allow(ctx, team.ID, lim, req.MaxTokens)
		switch {
		case err != nil:
			s.logger.Error("rate limiter unavailable; failing open", slog.String("team", team.ID), slog.Any("error", err))
		case !d.Allowed:
			body := map[string]any{
				"error":               "rate_limited",
				"dimension":           d.Dimension,
				"retry_after_seconds": int(d.RetryAfter / time.Second),
			}
			if d.ExceedsCapacity {
				body["message"] = fmt.Sprintf("max_tokens %d exceeds the team's tokens_per_minute of %d", req.MaxTokens, team.TokensPerMinute)
			} else {
				w.Header().Set("Retry-After", strconv.Itoa(int(d.RetryAfter/time.Second)))
			}
			writeJSON(w, http.StatusTooManyRequests, body)
			return false
		}
	}

	if s.budget != nil {
		lim := budget.Limits{DailyUSD: team.DailyBudgetUSD, MonthlyUSD: team.MonthlyBudgetUSD}
		st, err := s.budget.Check(ctx, team.ID, lim)
		if err != nil {
			s.logger.Error("budget store unavailable; failing open", slog.String("team", team.ID), slog.Any("error", err))
		} else if p, exceeded := st.Exceeded(); exceeded {
			writeJSON(w, http.StatusPaymentRequired, map[string]any{
				"error":     "budget_exhausted",
				"period":    p.Name,
				"limit_usd": p.Limit,
				"spent_usd": p.Spent,
				"resets_at": p.ResetsAt.Format(time.RFC3339),
			})
			return false
		}
	}
	return true
}

// settle reconciles the token reservation and records spend after a
// successful upstream call. Failures are logged, never surfaced: the client
// has its answer and the upstream has already billed for it.
func (s *Server) settle(ctx context.Context, team *config.Team, req *provider.Request, resp *provider.Response) {
	// Use a short independent deadline so a slow Redis cannot hold the response.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()

	if s.limiter != nil {
		lim := limiter.Limits{RequestsPerMinute: team.RequestsPerMinute, TokensPerMinute: team.TokensPerMinute}
		if err := s.limiter.Reconcile(ctx, team.ID, lim, req.MaxTokens, resp.InputTokens+resp.OutputTokens); err != nil {
			s.logger.Error("token reconciliation failed", slog.String("team", team.ID), slog.Any("error", err))
		}
	}
	if s.budget != nil {
		lim := budget.Limits{DailyUSD: team.DailyBudgetUSD, MonthlyUSD: team.MonthlyBudgetUSD}
		if _, err := s.budget.Record(ctx, team.ID, lim, resp.CostUSD); err != nil {
			s.logger.Error("spend recording failed", slog.String("team", team.ID), slog.Float64("cost_usd", resp.CostUSD), slog.Any("error", err))
		}
	}
}

// writeDispatchError maps routing and upstream failures to client responses.
func (s *Server) writeDispatchError(w http.ResponseWriter, err error) {
	var ce *router.ChainError
	switch {
	case errors.Is(err, router.ErrUnknownModel):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown_model"})
		return
	case errors.Is(err, context.DeadlineExceeded):
		body := map[string]any{"error": "upstream_timeout"}
		if errors.As(err, &ce) {
			body["attempted"] = ce.Attempts
		}
		writeJSON(w, http.StatusGatewayTimeout, body)
		return
	case errors.As(err, &ce):
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "upstream_unavailable", "attempted": ce.Attempts})
		return
	}

	var pe *provider.Error
	if errors.As(err, &pe) && pe.Class == provider.ErrNonRetryable {
		status := http.StatusBadGateway
		if pe.StatusCode == http.StatusBadRequest {
			// The upstream judged the request itself malformed; that is the caller's to fix.
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]string{"error": "upstream_rejected", "message": pe.Err.Error()})
		return
	}
	s.logger.Error("unclassified dispatch error", slog.Any("error", err))
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "upstream_unavailable"})
}
