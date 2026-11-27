package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/agents/claude"
)

func TestCloseInteractiveProcessEndsRunningProcess(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	start := time.Now()
	_ = closeInteractiveProcess(cmd, done)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("close took %v, want fast termination", elapsed)
	}
	if cmd.ProcessState == nil {
		t.Fatal("process state missing after close")
	}
	if cmd.ProcessState.Success() {
		t.Fatal("process should have been signaled, not exited cleanly")
	}
}

func TestClaudeReloginWaitsForNewCredential(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"old-access","scopes":["user:inference"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := claude.Store{Dir: t.TempDir()}
	runner := claudeRunner{store: store, out: io.Discard, errOut: io.Discard}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan bool, 1)
	go func() {
		_, closed := runner.runClaudeUntilCredential(ctx, exec.Command("sleep", "30"), dir)
		done <- closed
	}()
	select {
	case <-done:
		t.Fatal("login closed before a new credential was issued")
	case <-time.After(2500 * time.Millisecond):
	}
	if err := store.WriteCredential(ctx, dir, claude.CredentialInfo{AccessToken: "new-access", RefreshToken: "new-refresh", Scopes: []string{"user:inference", "user:profile"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case closed := <-done:
		if !closed {
			t.Fatal("login process exited without detecting the new credential")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("login did not close after the new credential was issued")
	}
}
