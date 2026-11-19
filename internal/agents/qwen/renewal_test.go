package qwen

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func renewalProfile(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := writeRawConsoleConfigIn(root, "qwen-token:work", map[string]any{
		"access_token": "expired", "access_key_id": "test-id", "access_key_secret": "test-secret",
		"console_region": defaultRegion, "console_site": defaultSite,
	}); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestConsoleTokenSignatureMatchesOfficialACS3Format(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://modelstudio.ap-southeast-1.aliyuncs.com/modelstudio/cli/generateAccessToken", nil)
	if err != nil {
		t.Fatal(err)
	}
	signConsoleTokenRequest(req, consoleConfig{AccessKeyID: "test-id", AccessKeySecret: "test-secret", SecurityToken: "sts-fixture"},
		"2026-11-19T00:00:00Z", "fixed-nonce")
	// Independent SHA256/HMAC vector computed from the vendor's canonical
	// request format, including the empty body and signed STS header.
	want := "ACS3-HMAC-SHA256 Credential=test-id,SignedHeaders=content-type;host;x-acs-action;x-acs-content-sha256;x-acs-date;x-acs-security-token;x-acs-signature-nonce;x-acs-version,Signature=c8bb259c00a569c5478f29ec524a95a70379d5bcc4338686d3372db869cfa689"
	if req.Header.Get("Authorization") != want {
		t.Fatal("signature differs from the official ACS3 format")
	}
}

func TestConsoleRenewalAcrossProcesses(t *testing.T) {
	if root := os.Getenv("SUBROUTER_QWEN_RENEWAL_TEST_ROOT"); root != "" {
		target, _ := url.Parse(os.Getenv("SUBROUTER_QWEN_RENEWAL_TEST_URL"))
		client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			forward := req.Clone(req.Context())
			copyURL := *req.URL
			copyURL.Scheme, copyURL.Host = target.Scheme, target.Host
			forward.URL = &copyURL
			return http.DefaultTransport.RoundTrip(forward)
		})}
		if _, err := FetchUsageIn(t.Context(), client, root, "qwen-token:work"); err != nil {
			t.Fatal(err)
		}
		return
	}
	root := renewalProfile(t)
	var minted, expired atomic.Int32
	ready := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/modelstudio/cli/generateAccessToken" {
			minted.Add(1)
			_, _ = io.WriteString(w, `{"cliAccessToken":"renewed"}`)
			return
		}
		if req.Header.Get("Authorization") == "Bearer expired" {
			if expired.Add(1) == 2 {
				close(ready)
			}
			select {
			case <-ready:
			case <-time.After(5 * time.Second):
				t.Error("both processes did not reach the expired-credential path")
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"per1MonthPercentage":0.2}`)
	}))
	defer server.Close()
	commands := make([]*exec.Cmd, 2)
	outputs := make([]bytes.Buffer, 2)
	for i := range commands {
		commands[i] = exec.Command(os.Args[0], "-test.run=^TestConsoleRenewalAcrossProcesses$")
		commands[i].Env = append(os.Environ(), "SUBROUTER_QWEN_RENEWAL_TEST_ROOT="+root, "SUBROUTER_QWEN_RENEWAL_TEST_URL="+server.URL)
		commands[i].Stdout, commands[i].Stderr = &outputs[i], &outputs[i]
		if err := commands[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for i, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("renewal process failed: %v\n%s", err, outputs[i].String())
		}
	}
	if minted.Load() != 1 {
		t.Fatalf("overlapping processes minted %d tokens", minted.Load())
	}
}

func TestAccessKeyEnrollmentRequiresPrivateFileAndRealQuota(t *testing.T) {
	path := filepath.Join(t.TempDir(), "renewal.json")
	if err := os.WriteFile(path, []byte(`{"access_key_id":"test-id","access_key_secret":"test-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/modelstudio/cli/generateAccessToken" {
			return renewalResponse(req, 200, `{"cliAccessToken":"renewed"}`), nil
		}
		return renewalResponse(req, 200, `{"per1MonthPercentage":0.1}`), nil
	})}
	credential, err := AuthorizeConsoleAccessKeyFile(t.Context(), client, path)
	if err != nil || credential.AccessToken != "renewed" || credential.AccessKeyID != "test-id" {
		t.Fatalf("native AccessKey enrollment failed: %v", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := AuthorizeConsoleAccessKeyFile(t.Context(), client, path); err == nil {
			t.Fatal("world-readable renewal credential was accepted")
		}
	}
}

func renewalResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

func TestExpiredConsoleCredentialRenewsAndReportsActualQuota(t *testing.T) {
	root := renewalProfile(t)
	minted := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/modelstudio/cli/generateAccessToken" {
			minted++
			if req.URL.Host != "modelstudio.ap-southeast-1.aliyuncs.com" ||
				req.Header.Get("x-acs-action") != "GenerateCLIAccessToken" ||
				!strings.HasPrefix(req.Header.Get("Authorization"), "ACS3-HMAC-SHA256 Credential=test-id,") {
				t.Error("token renewal did not use the vendor's signed OpenAPI endpoint")
			}
			return renewalResponse(req, 200, `{"cliAccessToken":"renewed"}`), nil
		}
		if req.Header.Get("Authorization") == "Bearer expired" {
			return renewalResponse(req, 401, `{"code":"BailianGateway.Login.NotLogined"}`), nil
		}
		if req.Header.Get("Authorization") != "Bearer renewed" {
			t.Error("unexpected console authorization")
		}
		return renewalResponse(req, 200, `{"per1MonthPercentage":0.375}`), nil
	})}
	usage, err := FetchUsageIn(context.Background(), client, root, "qwen-token:work")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Monthly == nil || usage.Monthly.UsedPercent != 37.5 || minted != 1 {
		t.Fatalf("renewal lost the real monthly quota: %+v, tokens minted=%d", usage, minted)
	}
	stored, err := readRawConsoleConfigIn(root, "qwen-token:work")
	if err != nil || stored["access_token"] != "renewed" || stored["access_key_secret"] != "test-secret" {
		t.Fatal("renewal was not durably saved without losing its renewal credential")
	}
	if _, err := FetchUsageIn(context.Background(), client, root, "qwen-token:work"); err != nil || minted != 1 {
		t.Fatal("fresh console credential was not reused")
	}
}

func TestConcurrentConsoleQuotaRefreshMintsOnlyOneToken(t *testing.T) {
	root := renewalProfile(t)
	var minted atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/modelstudio/cli/generateAccessToken" {
			minted.Add(1)
			return renewalResponse(req, 200, `{"cliAccessToken":"renewed"}`), nil
		}
		if req.Header.Get("Authorization") != "Bearer renewed" {
			return renewalResponse(req, 401, `{}`), nil
		}
		return renewalResponse(req, 200, `{"per1MonthPercentage":0.2}`), nil
	})}
	var group sync.WaitGroup
	for range 12 {
		group.Go(func() {
			if _, err := FetchUsageIn(t.Context(), client, root, "qwen-token:work"); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	if minted.Load() != 1 {
		t.Fatalf("concurrent status requests minted %d tokens", minted.Load())
	}
}

func TestRejectedRenewedQuotaDoesNotReplaceStoredCredential(t *testing.T) {
	root := renewalProfile(t)
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/modelstudio/cli/generateAccessToken" {
			return renewalResponse(req, 200, `{"cliAccessToken":"rejected"}`), nil
		}
		if req.Header.Get("Authorization") == "Bearer expired" {
			return renewalResponse(req, 401, `{}`), nil
		}
		return renewalResponse(req, 200, `{"data":{"success":false,"errorCode":"Forbidden"}}`), nil
	})}
	if _, err := FetchUsageIn(t.Context(), client, root, "qwen-token:work"); err == nil {
		t.Fatal("provider rejection was reported as quota")
	}
	stored, _ := readRawConsoleConfigIn(root, "qwen-token:work")
	if stored["access_token"] != "expired" {
		t.Fatal("unvalidated token replaced the stored authorization")
	}
}
