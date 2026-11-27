package claude

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestLimitResetsFromUsagePreservesGrantScopes(t *testing.T) {
	var usage UsageResponse
	if err := json.Unmarshal([]byte(`{"cedar_ember":{"eligible":true,"at_limit":true,"weekly_resets_at":"2030-01-01T00:00:00Z","grants":[{"id":"fable","label":"Fable reset","resets_left":2,"resets_total":3,"ends_at":"2029-12-31T00:00:00Z","clears":["seven_day_overage_included"],"usable_now":true,"gives_back_weekly":true},{"id":"full","label":"Full reset","type":"full","resets_left":1,"resets_total":1,"clears":["five_hour","seven_day"],"usable_now":false,"unavailable_reason":"needs_limit","use_requires_limit":true}]}}`), &usage); err != nil {
		t.Fatal(err)
	}
	got := LimitResetsFromUsage(&usage)
	if !got.Known || got.Status != "live" || got.Eligible == nil || !*got.Eligible || len(got.Grants) != 2 {
		t.Fatalf("status=%+v", got)
	}
	fable, full := got.Grants[0], got.Grants[1]
	if fable.Remaining != 2 || !fable.UsableNow || !fable.GivesBackWeekly || len(fable.Clears) != 1 || fable.Clears[0] != "seven_day_overage_included" || fable.ExpiresAt != "2029-12-31T00:00:00Z" {
		t.Fatalf("Fable grant=%+v", fable)
	}
	if full.Kind != "full" || full.UsableNow || !full.RequiresLimit || full.UnavailableReason != "needs_limit" {
		t.Fatalf("full grant=%+v", full)
	}
}

func TestLimitResetUnknownIsNotZeroAvailable(t *testing.T) {
	for _, body := range []string{`{}`, `{"cedar_ember":null}`, `{"cedar_ember":{}}`, `{"cedar_ember":{"eligible":true}}`, `{"cedar_ember":{"eligible":true,"grants":null}}`, `{"cedar_ember":{"eligible":true,"grants":[{"id":"bad","resets_left":-1}]}}`, `{"cedar_ember":{"eligible":true,"grants":[{"id":"missing-count"}]}}`} {
		var usage UsageResponse
		if err := json.Unmarshal([]byte(body), &usage); err != nil {
			t.Fatal(err)
		}
		if got := LimitResetsFromUsage(&usage); got.Known || got.Status != "unavailable" {
			t.Fatalf("body=%s status=%+v", body, got)
		}
	}
	var usage UsageResponse
	if err := json.Unmarshal([]byte(`{"cedar_ember":{"eligible":false,"ineligible_reason":"no_grant"}}`), &usage); err != nil {
		t.Fatal(err)
	}
	if got := LimitResetsFromUsage(&usage); !got.Known || got.Eligible == nil || *got.Eligible || got.Reason != "no_grant" {
		t.Fatalf("ineligible=%+v", got)
	}
}

type resetUsageTransport func(*http.Request) (*http.Response, error)

func (f resetUsageTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFetchUsageOptsIntoResetStatusAndKeepsSpend(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: resetUsageTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "GET" || r.URL.Path != "/api/oauth/usage" || r.URL.Query().Get("cedar_ember") != "1" || r.URL.Query().Has("skip_spend") {
			t.Fatalf("request=%s %s", r.Method, r.URL)
		}
		if r.Header.Get("User-Agent") != "claude-cli/2.1.316 (external, cli)" || r.Header.Get("X-App") != "cli" {
			t.Fatalf("reset status requires a supported Claude client identity, got user-agent=%q x-app=%q", r.Header.Get("User-Agent"), r.Header.Get("X-App"))
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"seven_day":{"utilization":3},"extra_usage":{"is_enabled":true,"used_credits":7},"spend":{"balance":1234},"cedar_ember":{"eligible":true,"grants":[]}}`)), Request: r}, nil
	})}
	usage, err := FetchUsage(context.Background(), client, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || usage.ExtraUsage == nil || usage.Spend == nil || usage.SevenDay == nil || !LimitResetsFromUsage(usage).Known {
		t.Fatalf("usage=%+v calls=%d", usage, calls)
	}
}
