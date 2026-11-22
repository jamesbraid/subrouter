package proxy

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// A pre-send account change rebuilds the body through GetBody. The upload
// gate must follow the physical body, not keep a permit on the discarded one.
func TestUploadGateReleasesAfterPreSendAccountRetarget(t *testing.T) {
	server := upstreamStackTestServer(t, &url.URL{Scheme: "https", Host: "pool.invalid"}, nil, 2)
	server.SchedulerRef.MarkAccountUnavailableUntil(accounts.ProviderCodex, "codex-account-0", time.Now().Add(time.Hour))
	body := strings.Repeat("x", replayablePostLimiterMinBytes)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://pool.invalid/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer oauth-token-0")
	limiter := make(chan struct{}, 1)
	physical := 0
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		physical++
		if upstreamStackToken(req) == "oauth-token-0" {
			t.Fatal("blocked account was sent upstream")
		}
		if _, err := io.Copy(io.Discard, req.Body); err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: http.NoBody}, nil
	})
	attempt := &upstreamAttempt{server: &server, provider: accounts.ProviderCodex, account: server.Accounts[0], agent: "codex", session: "upload-retarget", poolModel: "gpt-6.2-astra", budget: newAttemptBudget(3), getBody: request.GetBody, path: "/v1/responses"}
	transport := upstreamLayers{
		usageLimit:     &usageLimitRetryTransport{method: http.MethodPost, maxAttempts: 3},
		replayablePost: &replayablePostRetryTransport{method: http.MethodPost, maxAttempts: 1, limiter: limiter},
	}.build(base, attempt)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if physical != 1 {
		t.Fatalf("physical attempts = %d, want 1", physical)
	}
	if len(limiter) != 0 {
		t.Fatal("pre-send retarget leaked an upload permit after the physical upload completed")
	}
}
