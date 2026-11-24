package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// Anthropic answers "Your credit balance is too low" for two different
// reasons. One is a key whose prepaid credit is spent; that belongs to the
// key, and failing over to the next key is right. The other is a request the
// key's credit will not pay for, whatever the balance: on 2026-11-23 eight
// funded keys refused every request that identified itself as Claude Code
// (the claude-code beta header, or a first system block that is exactly a
// Claude Code or Agent SDK identity line) and accepted the same request
// without it. Failover then walked each request through every key, cooled
// them all, and the pool answered 503.
//
// A second key refusing the same request is the evidence that the refusal
// follows the request. subrouter then stops failing over, gives the first
// key back to routing, and answers with its own non-retryable explanation.
//
// subrouter never strips or adds the identity to change how Anthropic bills
// a request (see claude_third_party.go); the client must send the identity
// that is true of it.

const claudeCodeBetaHeader = "claude-code-20250219"

// claudeIdentityLines are system prompts by which a request identifies
// itself as Claude Code or an Agent SDK app. Anthropic matches the first
// system block exactly: the same line followed by more text does not count.
var claudeIdentityLines = []string{
	"You are Claude Code, Anthropic's official CLI for Claude.",
	"You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.",
	"You are a Claude agent, built on Anthropic's Claude Agent SDK.",
}

const claudeRequestCreditRefusalIdentityReply = "subrouter: Anthropic refused this request on two API keys with \"credit balance is too low\", and those keys accept other requests. This request identifies itself as Claude Code (the claude-code beta header, or a first system block that is the Claude Code or Agent SDK identity line), and Anthropic does not bill such requests to these keys' credit. If this client is not Claude Code, stop sending that identity."

const claudeRequestCreditRefusalReply = "subrouter: Anthropic refused this request on two API keys with \"credit balance is too low\", so the refusal follows the request rather than the keys. Retrying it will fail the same way."

// releaseClaudeCreditCooldown ends a credit cooldown that a request, not the
// key, caused.
func (s Server) releaseClaudeCreditCooldown(accountID string) {
	if s.SchedulerRef == nil || accountID == "" {
		return
	}
	s.SchedulerRef.MarkCreditExhaustedUntil(schedulerAccountProvider(accounts.ProviderClaude), accountID, time.Now())
}

// claudeCodeIdentified reports whether req identifies itself as Claude Code
// or an Agent SDK app, by Anthropic's own markers.
func claudeCodeIdentified(req *http.Request) bool {
	if req == nil {
		return false
	}
	for _, value := range req.Header.Values("Anthropic-Beta") {
		for part := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), claudeCodeBetaHeader) {
				return true
			}
		}
	}
	if req.GetBody == nil {
		return false
	}
	body, err := req.GetBody()
	if err != nil {
		return false
	}
	defer body.Close()
	raw, err := io.ReadAll(body)
	if err != nil {
		return false
	}
	return claudeSystemIdentified(raw)
}

func claudeSystemIdentified(body []byte) bool {
	var request struct {
		System json.RawMessage `json:"system"`
	}
	if json.Unmarshal(body, &request) != nil || len(request.System) == 0 {
		return false
	}
	var first string
	var text string
	if json.Unmarshal(request.System, &text) == nil {
		first = text
	} else {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(request.System, &blocks) != nil || len(blocks) == 0 || blocks[0].Type != "text" {
			return false
		}
		first = blocks[0].Text
	}
	return slices.Contains(claudeIdentityLines, strings.TrimSpace(first))
}

// claudeRequestCreditRefusalResponse replaces the upstream refusal with
// subrouter's explanation: a 400 in Anthropic's error shape, marked
// non-retryable. Anthropic's request ID stays in a header for support.
// identified is claudeCodeIdentified of the client's request: the upstream
// attempt's body has already been sent and cannot be read again.
func claudeRequestCreditRefusalResponse(upstream *http.Response, req *http.Request, identified bool) *http.Response {
	status := http.StatusBadRequest
	requestID := ""
	if upstream != nil {
		requestID = upstreamRequestID(upstream.Header)
		if upstream.Request != nil {
			req = upstream.Request
		}
		if upstream.Body != nil {
			_ = upstream.Body.Close()
		}
	}
	message := claudeRequestCreditRefusalReply
	if identified {
		message = claudeRequestCreditRefusalIdentityReply
	}
	body, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": "invalid_request_error", "message": message},
	})
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	header.Set("Content-Length", strconv.Itoa(len(body)))
	header.Set("X-Should-Retry", "false")
	header.Set("X-Subrouter-Error", "claude_request_credit_refusal")
	if requestID != "" {
		header.Set("Request-Id", requestID)
	}
	return &http.Response{
		StatusCode:    status,
		Status:        strconv.Itoa(status) + " " + http.StatusText(status),
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}
