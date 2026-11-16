package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func shutdownWebSocketServer(t *testing.T) (*httptest.Server, *proxy.Lifecycle, *websocket.Conn) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var request map[string]any
			if err := conn.ReadJSON(&request); err != nil {
				return
			}
			if err := conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{
				"id": request["input"], "status": "completed", "output": []any{},
			}}); err != nil {
				return
			}
		}
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, _ := url.Parse(upstream.URL)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := proxy.NewLifecycle()
	server := httptest.NewServer(proxy.Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{ID: "fixture", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "fake-token"}},
		Sessions: store, Scheduler: selectacct.NewScheduler(nil), Lifecycle: lifecycle, MaxBodyBytes: 1 << 20,
	}.Handler())
	t.Cleanup(server.Close)
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{
		"X-Subrouter-Account-ID": {"fixture"}, "X-Subrouter-Agent": {"codex"}, "X-Subrouter-Session": {"shutdown-fixture"},
	})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return server, lifecycle, conn
}

func assertShutdownTurn(t *testing.T, conn *websocket.Conn, id string) {
	t.Helper()
	if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "gpt-6.1", "input": id}); err != nil {
		t.Fatal(err)
	}
	_, body, err := conn.ReadMessage()
	if err != nil || !strings.Contains(string(body), id) {
		t.Fatalf("WebSocket turn failed: %q, %v", body, err)
	}
}

func TestShutdownWaitsForEstablishedWebSocket(t *testing.T) {
	server, lifecycle, conn := shutdownWebSocketServer(t)
	assertShutdownTurn(t, conn, "before-stop")
	errCh := make(chan error, 1)
	errCh <- nil
	done := make(chan error, 1)
	go func() { done <- shutdownOnSignal(server.Config, lifecycle, syscall.SIGTERM, time.Second, nil, errCh) }()
	select {
	case err := <-done:
		t.Fatalf("shutdown returned while its WebSocket was still connected: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	assertShutdownTurn(t, conn, "during-stop")
	_ = conn.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after the WebSocket closed")
	}
}

func TestShutdownWebSocketDrainUsesTheConfiguredDeadline(t *testing.T) {
	server, lifecycle, conn := shutdownWebSocketServer(t)
	assertShutdownTurn(t, conn, "held")
	errCh := make(chan error, 1)
	errCh <- nil
	started := time.Now()
	err := shutdownOnSignal(server.Config, lifecycle, syscall.SIGTERM, 60*time.Millisecond, nil, errCh)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v, want the drain deadline", err)
	}
	if elapsed := time.Since(started); elapsed < 40*time.Millisecond || elapsed > time.Second {
		t.Fatalf("drain used the wrong deadline: %v", elapsed)
	}
}
