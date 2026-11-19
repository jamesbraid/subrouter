package qwen

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

// AuthorizeConsoleAccessKeyFile verifies a privately staged AccessKey pair by
// generating a console token and reading actual Token Plan usage before it is
// saved or uploaded. It never changes the inference API key.
func AuthorizeConsoleAccessKeyFile(ctx context.Context, client *http.Client, path string) (ConsoleCredential, error) {
	file, err := os.Open(path)
	if err != nil {
		return ConsoleCredential{}, fmt.Errorf("open Alibaba renewal credential: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ConsoleCredential{}, err
	}
	if !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return ConsoleCredential{}, errors.New("Alibaba renewal credential must be a private regular file (mode 0600)")
	}
	body, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil || len(body) > 64<<10 {
		return ConsoleCredential{}, errors.New("could not read Alibaba renewal credential")
	}
	var credential ConsoleCredential
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&credential) != nil || decoder.Decode(new(any)) != io.EOF || credential.AccessToken != "" {
		return ConsoleCredential{}, errors.New("invalid Alibaba renewal credential JSON; provide access_key_id and access_key_secret")
	}
	if credential.ConsoleRegion == "" {
		credential.ConsoleRegion = defaultRegion
	}
	if credential.ConsoleSite == "" {
		credential.ConsoleSite = defaultSite
	}
	config := consoleConfig{AccessKeyID: credential.AccessKeyID, AccessKeySecret: credential.AccessKeySecret,
		SecurityToken: credential.SecurityToken, ConsoleRegion: credential.ConsoleRegion,
		ConsoleSite: credential.ConsoleSite, ConsoleSwitchAgent: credential.ConsoleSwitchAgent}
	credential.AccessToken, err = generateConsoleToken(ctx, client, config)
	if err != nil {
		return ConsoleCredential{}, err
	}
	config.AccessToken = credential.AccessToken
	var response any
	if err := callConsole(ctx, client, config, usageAPI, nil, &response); err != nil {
		return ConsoleCredential{}, err
	}
	if _, err := findUsagePayload(response); err != nil {
		return ConsoleCredential{}, err
	}
	return credential, nil
}

// withConsoleCredential retries an expired console session once using the
// official GenerateCLIAccessToken API. The business request must succeed
// before the new token is persisted. All writers share the account file lock.
func withConsoleCredential(ctx context.Context, client *http.Client, root, accountID string, request func(consoleConfig) error) error {
	config, err := readConsoleConfigIn(root, accountID)
	if err != nil {
		return err
	}
	err = ErrConsoleLoginRequired
	if strings.TrimSpace(config.AccessToken) != "" {
		err = request(config)
	}
	if !errors.Is(err, ErrConsoleLoginRequired) {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	lock, err := lockConsoleCredential(root, accountID)
	if err != nil {
		return err
	}
	defer lock.Close()
	current, err := readConsoleConfigIn(root, accountID)
	if err != nil {
		return err
	}
	if current.AccessToken != "" && current.AccessToken != config.AccessToken {
		if err := request(current); !errors.Is(err, ErrConsoleLoginRequired) {
			return err
		}
	}
	if current.AccessKeyID == "" || current.AccessKeySecret == "" {
		return ErrConsoleLoginRequired
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	token, err := generateConsoleToken(ctx, client, current)
	if err != nil {
		return err
	}
	current.AccessToken = token
	if err := request(current); err != nil {
		return err
	}
	raw, err := readExistingRawConsoleConfigIn(root, accountID)
	if err != nil {
		return err
	}
	raw["access_token"] = token
	if err := writeRawConsoleConfigIn(root, accountID, raw); err != nil {
		return err
	}
	return syncConsoleCredentialDurably(root, accountID)
}

func generateConsoleToken(ctx context.Context, client *http.Client, config consoleConfig) (string, error) {
	region := config.ConsoleRegion
	if region == "" {
		region = defaultRegion
	}
	if region != "ap-southeast-1" && region != "cn-beijing" {
		return "", errors.New("unsupported Alibaba console renewal region")
	}
	if err := validateConsoleAccessKey(config.AccessKeyID, config.AccessKeySecret, config.SecurityToken); err != nil {
		return "", err
	}
	host := "modelstudio." + region + ".aliyuncs.com"
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+host+"/modelstudio/cli/generateAccessToken", nil)
	if err != nil {
		return "", err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	signConsoleTokenRequest(req, config, time.Now().UTC().Format("2006-01-02T15:04:05Z"), hex.EncodeToString(nonce[:]))
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	secured := *client
	secured.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := secured.Do(req)
	if err != nil {
		return "", fmt.Errorf("renew Alibaba console token: %w", err)
	}
	defer res.Body.Close()
	var result struct {
		Token   string `json:"cliAccessToken"`
		Success *bool  `json:"Success"`
		Code    string `json:"Code"`
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil {
		return "", fmt.Errorf("read Alibaba console renewal: %w", err)
	}
	if len(body) > 1<<20 || json.Unmarshal(body, &result) != nil {
		return "", errors.New("invalid Alibaba console renewal response")
	}
	if res.StatusCode != http.StatusOK || result.Success != nil && !*result.Success {
		// Provider messages can contain credential material. Return only its
		// status and bounded error code, never the raw response or headers.
		code := result.Code
		if len(code) > 128 || strings.IndexFunc(code, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
			code = "unknown"
		}
		return "", fmt.Errorf("Alibaba console renewal rejected: HTTP %d (%s)", res.StatusCode, code)
	}
	if result.Token == "" || len(result.Token) > 16384 || strings.IndexFunc(result.Token, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
		return "", errors.New("Alibaba console renewal omitted a valid token")
	}
	return result.Token, nil
}

// ACS3 signing follows Alibaba's official CLI, including an empty POST body
// and the optional STS token in the signed headers. No vendor CLI is invoked.
func signConsoleTokenRequest(req *http.Request, config consoleConfig, date, nonce string) {
	empty := sha256.Sum256(nil)
	bodyHash := hex.EncodeToString(empty[:])
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-acs-action", "GenerateCLIAccessToken")
	req.Header.Set("x-acs-version", "2026-02-10")
	req.Header.Set("x-acs-date", date)
	req.Header.Set("x-acs-signature-nonce", nonce)
	req.Header.Set("x-acs-content-sha256", bodyHash)
	keys := []string{"content-type", "host", "x-acs-action", "x-acs-content-sha256", "x-acs-date"}
	if config.SecurityToken != "" {
		req.Header.Set("x-acs-security-token", config.SecurityToken)
		keys = append(keys, "x-acs-security-token")
	}
	keys = append(keys, "x-acs-signature-nonce", "x-acs-version")
	var headers strings.Builder
	for _, key := range keys {
		value := req.Header.Get(key)
		if key == "host" {
			value = req.URL.Host
		}
		fmt.Fprintf(&headers, "%s:%s\n", key, value)
	}
	signed := strings.Join(keys, ";")
	canonical := strings.Join([]string{http.MethodPost, req.URL.Path, "", headers.String(), signed, bodyHash}, "\n")
	hash := sha256.Sum256([]byte(canonical))
	mac := hmac.New(sha256.New, []byte(config.AccessKeySecret))
	_, _ = mac.Write([]byte("ACS3-HMAC-SHA256\n" + hex.EncodeToString(hash[:])))
	req.Header.Set("Authorization", "ACS3-HMAC-SHA256 Credential="+config.AccessKeyID+",SignedHeaders="+signed+",Signature="+hex.EncodeToString(mac.Sum(nil)))
}

func validateConsoleAccessKey(id, secret, securityToken string) error {
	if id == "" || secret == "" {
		return errors.New("Alibaba quota renewal requires both access_key_id and access_key_secret")
	}
	for _, value := range []string{id, secret, securityToken} {
		if len(value) > 16384 || strings.IndexFunc(value, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
			return errors.New("invalid Alibaba quota renewal credential")
		}
	}
	return nil
}
