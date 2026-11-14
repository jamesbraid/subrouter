package qwensubscription

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/fsutil"
)

var webProfileURL = "https://auth.qwen.ai/api/v2/auths/"
var quotaURL = "https://sg-entitlement.qwen.com/api/entitlement/agent/usage"

var ErrQuotaLoginRequired = errors.New("Qwen quota login required; run sr qwen subscription quota-login <label>")

// quotaCredential is independent of the rotating inference grant. It is only
// sent to Qwen's fixed profile and entitlement endpoints, never to inference.
type quotaCredential struct {
	AccessToken string `json:"access_token"`
	Subject     string `json:"subject"`
	Email       string `json:"email"`
}

func (s Store) quotaPath(label string) (string, error) {
	path, err := s.path(label)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.dir(), "quota", filepath.Base(path)), nil
}

func normalizeQuotaToken(raw string) (string, error) {
	token := strings.TrimSpace(raw)
	if len(token) > 7 && strings.EqualFold(token[:7], "Bearer ") {
		token = strings.TrimSpace(token[7:])
	}
	if token == "" || len(token) > 16384 {
		return "", errors.New("invalid Qwen quota authorization")
	}
	for _, r := range token {
		if r < 33 || r > 126 {
			return "", errors.New("invalid Qwen quota authorization")
		}
	}
	return token, nil
}

// webJSON deliberately treats HTTP-200 business authentication errors as
// failures and never includes a response body, URL query, or token in errors.
func webJSON(ctx context.Context, client *http.Client, endpoint, token string) (json.RawMessage, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, errors.New("invalid Qwen quota endpoint")
	}
	query := parsed.Query()
	query.Set("biz_id", "ai_qwen")
	query.Set("chat_client", "h5")
	query.Set("device", "pc")
	query.Set("pr", "qwen-i18n")
	parsed.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, errors.New("could not prepare Qwen quota request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "subrouter")
	req.Header.Set("Accept", "application/json")
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	checked := *client
	if checked.Timeout == 0 {
		checked.Timeout = 15 * time.Second
	}
	checked.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := checked.Do(req)
	if err != nil {
		return nil, errors.New("Qwen quota request failed")
	}
	defer res.Body.Close()
	if res.StatusCode == 401 || res.StatusCode == 403 {
		return nil, ErrQuotaLoginRequired
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Qwen quota returned HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return nil, errors.New("invalid Qwen quota response")
	}
	var envelope struct {
		Success   *bool           `json:"success"`
		HTTPCode  int             `json:"httpCode"`
		ErrorCode string          `json:"errorCode"`
		Data      json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return nil, errors.New("invalid Qwen quota response")
	}
	if envelope.HTTPCode == 401 || envelope.HTTPCode == 403 || envelope.ErrorCode == "NOT_LOGIN" {
		return nil, ErrQuotaLoginRequired
	}
	if envelope.Success != nil && !*envelope.Success {
		return nil, errors.New("Qwen quota request was rejected")
	}
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		return envelope.Data, nil
	}
	return body, nil
}

func verifyQuotaOwner(ctx context.Context, client *http.Client, token, expectedEmail string) error {
	if strings.TrimSpace(expectedEmail) == "" {
		return errors.New("enrolled Qwen account has no verified email")
	}
	body, err := webJSON(ctx, client, webProfileURL, token)
	if err != nil {
		return err
	}
	var profile struct {
		Email string `json:"email"`
	}
	if json.Unmarshal(body, &profile) != nil || strings.TrimSpace(profile.Email) == "" {
		return errors.New("Qwen quota sign-in has no verified email")
	}
	if !strings.EqualFold(strings.TrimSpace(profile.Email), strings.TrimSpace(expectedEmail)) {
		return errors.New("Qwen quota sign-in belongs to another account")
	}
	return nil
}

func fetchQuota(ctx context.Context, client *http.Client, token string, now time.Time) ([]accounts.UsageWindow, error) {
	body, err := webJSON(ctx, client, quotaURL, token)
	if err != nil {
		return nil, err
	}
	var usage struct {
		RemainingPercent *float64 `json:"remainingPercent"`
		ResetAt          *int64   `json:"resetAt"`
	}
	if json.Unmarshal(body, &usage) != nil || usage.RemainingPercent == nil || usage.ResetAt == nil {
		return nil, errors.New("Qwen quota response omitted remaining percentage or reset")
	}
	remaining := *usage.RemainingPercent
	if math.IsNaN(remaining) || math.IsInf(remaining, 0) || remaining < 0 || remaining > 100 {
		return nil, errors.New("Qwen quota response has an invalid percentage")
	}
	// Qwen's web app feeds resetAt directly to Date: it is epoch milliseconds.
	reset := time.UnixMilli(*usage.ResetAt).UTC()
	if !reset.After(now) || reset.After(now.Add(8*24*time.Hour)) {
		return nil, errors.New("Qwen weekly quota reset is stale or invalid")
	}
	return []accounts.UsageWindow{{Name: "weekly", UsedPercent: 100 - remaining, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAt: reset, ResetAfterSeconds: int64(reset.Sub(now).Seconds())}}, nil
}

// EnrollQuota validates identity and the live quota before replacing a saved
// telemetry credential. The caller owns the account mutation transaction.
func (s Store) EnrollQuota(ctx context.Context, client *http.Client, label, rawToken string) error {
	credential, ok, err := s.read(label)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("Qwen subscription account was not found")
	}
	if credential.VerificationPending {
		return errors.New("Qwen subscription awaits identity verification")
	}
	token, err := normalizeQuotaToken(rawToken)
	if err != nil {
		return err
	}
	if err := verifyQuotaOwner(ctx, client, token, credential.Email); err != nil {
		return err
	}
	if _, err := fetchQuota(ctx, client, token, time.Now()); err != nil {
		return err
	}
	path, err := s.quotaPath(label)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(quotaCredential{AccessToken: token, Subject: credential.Subject, Email: credential.Email})
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, body, 0o600)
}

func (s Store) RemoveQuota(label string) error {
	path, err := s.quotaPath(label)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return fsutil.SyncDir(filepath.Dir(path))
}

func (s Store) quotaWindows(ctx context.Context, client *http.Client, label string, credential CredentialInfo) ([]accounts.UsageWindow, error) {
	path, err := s.quotaPath(label)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, ErrQuotaLoginRequired
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 32<<10 {
		return nil, errors.New("invalid Qwen quota credential file")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var quota quotaCredential
	if json.Unmarshal(body, &quota) != nil || quota.Subject != credential.Subject || !strings.EqualFold(quota.Email, credential.Email) {
		return nil, ErrQuotaLoginRequired
	}
	token, err := normalizeQuotaToken(quota.AccessToken)
	if err != nil {
		return nil, ErrQuotaLoginRequired
	}
	if err := verifyQuotaOwner(ctx, client, token, credential.Email); err != nil {
		return nil, err
	}
	return fetchQuota(ctx, client, token, time.Now())
}
