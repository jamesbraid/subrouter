package proxy

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestUpgradeClaudeRequestCacheTTL(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    string
		changed bool
	}{
		{
			name:    "bare ephemeral cache",
			body:    `{"system":[{"text":"rules","cache_control":{"type":"ephemeral"}}]}`,
			want:    `{"system":[{"text":"rules","cache_control":{"type":"ephemeral","ttl":"1h"}}]}`,
			changed: true,
		},
		{
			name:    "all bare breakpoints",
			body:    `{"system":[{"cache_control":{"type":"ephemeral"}}],"tools":[{"cache_control":{"type":"ephemeral"}}]}`,
			want:    `{"system":[{"cache_control":{"type":"ephemeral","ttl":"1h"}}],"tools":[{"cache_control":{"type":"ephemeral","ttl":"1h"}}]}`,
			changed: true,
		},
		{
			name: "explicit ttl",
			body: `{"cache_control":{"type":"ephemeral","ttl":"5m"}}`,
			want: `{"cache_control":{"type":"ephemeral","ttl":"5m"}}`,
		},
		{
			name: "escaped content",
			body: `{"text":"escaped \\"cache_control\\":{\\"type\\":\\"ephemeral\\"}"}`,
			want: `{"text":"escaped \\"cache_control\\":{\\"type\\":\\"ephemeral\\"}"}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(test.body))
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := makeRequestBodyReplayable(req, 1<<20); err != nil || !ok {
				t.Fatalf("make replayable = %v, %v", ok, err)
			}
			changed, err := upgradeClaudeRequestCacheTTL(req)
			if err != nil {
				t.Fatal(err)
			}
			if changed != test.changed {
				t.Fatalf("changed = %v, want %v", changed, test.changed)
			}
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(body, []byte(test.want)) {
				t.Fatalf("body = %s, want %s", body, test.want)
			}
			replay, err := req.GetBody()
			if err != nil {
				t.Fatal(err)
			}
			defer replay.Close()
			replayed, err := io.ReadAll(replay)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(replayed, []byte(test.want)) {
				t.Fatalf("replayed body = %s, want %s", replayed, test.want)
			}
			if req.ContentLength != int64(len(test.want)) {
				t.Fatalf("content length = %d, want %d", req.ContentLength, len(test.want))
			}
		})
	}
}
