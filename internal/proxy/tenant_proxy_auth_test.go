package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/tenant"
)

func TestTenantProxyUsesTenantKeyWithProtectedDefaultPool(t *testing.T) {
	registry := tenant.NewRegistry(t.TempDir())
	created, key, err := registry.Create("api")
	if err != nil {
		t.Fatal(err)
	}
	writeTenantAPIKeyAccount(t, registry, created.ID, "apikey:console", "sk-tenant-only")
	upstream, _ := url.Parse("https://provider.invalid")
	calls := 0
	base := Server{
		APIUpstream:     upstream,
		LocalProxyToken: "global-proxy-secret",
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.Header.Get("Authorization") != "Bearer sk-tenant-only" {
				t.Fatal("tenant request did not use its own provider credential")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"response-fixture"}`))}, nil
		}),
	}
	multi := &MultiTenant{Base: base, Registry: registry}
	handler := multi.Handler(base.Handler())
	for _, auth := range []string{key, "srt_00000000000000000000000000000000", ""} {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","input":"hello"}`))
		req.Header.Set("Authorization", "Bearer "+auth)
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if auth == key {
			if response.Code != 200 {
				t.Fatalf("tenant proxy status = %d, want 200; body=%s", response.Code, response.Body.String())
			}
		} else if response.Code != http.StatusUnauthorized {
			t.Fatalf("invalid/missing key status=%d, want 401", response.Code)
		}
	}
	if calls != 1 {
		t.Fatalf("physical requests = %d, want only the authenticated tenant request", calls)
	}
}

func TestTenantCannotSpendOperatorMeteredFallbacks(t *testing.T) {
	for _, backend := range []string{"anthropic", "openai", "bedrock"} {
		t.Run(backend, func(t *testing.T) {
			registry := tenant.NewRegistry(t.TempDir())
			_, key, err := registry.Create("empty-api")
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"fixture","type":"message","content":[],"output":[]}`))}, nil
			})
			base := Server{Transport: transport, Sessions: newSessionStore(t), MaxBodyBytes: 4096}
			path, body := "/v1/messages", `{"model":"claude-fable-5-2","max_tokens":8,"messages":[{"role":"user","content":"ping"}]}`
			switch backend {
			case "anthropic":
				base.ClaudeFableAPIKey = "operator-key"
			case "openai":
				endpoint, _ := url.Parse("https://operator.openai.azure.com/openai/v1")
				base.AzureCodex = &AzureCodexConfig{Endpoints: []AzureCodexEndpoint{{Name: "operator", BaseURL: endpoint, APIKey: "operator-key"}}, Transport: transport}
				path, body = "/v1/responses", `{"model":"gpt-test","input":"ping"}`
			case "bedrock":
				base.Bedrock = &BedrockConfig{Regions: []string{"us-east-1"}, Credentials: staticBedrockCreds(), Transport: transport}
				base.FableBedrockPrimary = true
			}
			multi := &MultiTenant{Base: base, Registry: registry}
			handler := multi.Handler(base.Handler())
			for _, tenantRequest := range []bool{false, true} {
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				if tenantRequest {
					req.Header.Set("Authorization", "Bearer "+key)
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, req)
				if !tenantRequest {
					if response.Code != 200 || calls != 1 {
						t.Fatalf("operator fallback was not usable: status=%d calls=%d body=%s", response.Code, calls, response.Body.String())
					}
				} else if response.Code == 200 || calls != 1 {
					t.Fatalf("empty tenant spent operator fallback: status=%d calls=%d", response.Code, calls)
				}
			}
		})
	}
}
