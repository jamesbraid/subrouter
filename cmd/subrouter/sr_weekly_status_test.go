package main

import (
	"bytes"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"strings"
	"testing"
)

func TestClaudeStatusDoesNotPromiseConsumptionOrder(t *testing.T) {
	var out bytes.Buffer
	rows := []srUsageRow{{email: "profile@example.com", provider: accounts.ProviderClaude, active: true, authMode: accounts.AuthModeOAuth, costTier: "plan", costTierRank: 1}}
	displayUsageRowsPerGroup(&out, rows)
	text := out.String()
	if !strings.Contains(text, "Rows show status, not routing order") || !strings.Contains(text, "saved profile") {
		t.Fatalf("status misrepresents profile selection as routing: %s", text)
	}
	if strings.Contains(text, "spend order:") {
		t.Fatalf("status still suggests row consumption order: %s", text)
	}
}
