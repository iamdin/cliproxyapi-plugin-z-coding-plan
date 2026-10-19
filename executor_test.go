package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPassthroughSendsOriginalBytesWithOnlyKeySwapped(t *testing.T) {
	var gotBody []byte
	var gotHeader http.Header
	var gotURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHeader, gotURL = r.Header.Clone(), r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1"}`))
	}))
	defer server.Close()
	mu.Lock()
	messagesEndpoint = server.URL + "/api/anthropic/v1/messages"
	mu.Unlock()
	t.Cleanup(func() { messagesEndpoint = defaultMessagesEndpoint })

	original := []byte("{\"model\":\"glm-5.3\",  \"thinking\":{\"type\":\"enabled\",\"budget_tokens\":8192},\n\"tools\":[{\"name\":\"bash\"}]}")
	raw, _ := json.Marshal(map[string]any{
		"OriginalRequest": original,
		"StorageJSON":     []byte(`{"type":"z-coding-plan","api_key":"zhipu-key"}`),
		"Headers": http.Header{
			"User-Agent": {"pi (linux)"}, "Anthropic-Beta": {"interleaved-thinking-2025-05-14"},
			"X-Api-Key": {"cpa-client-key"}, "Authorization": {"Bearer cpa-client-key"},
			"X-Forwarded-For": {"1.2.3.4"}, "Accept-Encoding": {"br"},
		},
	})
	var env envelope
	if err := json.Unmarshal(execute(raw), &env); err != nil || !env.OK {
		t.Fatalf("execute failed: %s", env.Error.Message)
	}
	if !bytes.Equal(gotBody, original) {
		t.Fatalf("body changed:\n%s\n%s", gotBody, original)
	}
	if gotURL != "/api/anthropic/v1/messages" {
		t.Fatalf("url changed: %s", gotURL)
	}
	if gotHeader.Get("X-Api-Key") != "zhipu-key" || gotHeader.Get("Authorization") != "" {
		t.Fatalf("auth not swapped: %v", gotHeader)
	}
	if gotHeader.Get("User-Agent") != "pi (linux)" || gotHeader.Get("Anthropic-Beta") != "interleaved-thinking-2025-05-14" {
		t.Fatalf("client headers lost: %v", gotHeader)
	}
	if gotHeader.Get("X-Forwarded-For") != "" {
		t.Fatalf("hop header leaked: %v", gotHeader)
	}
}

func TestPassthroughReturnsZhipuErrorWithStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"1308","message":"已达到 5 小时的使用上限。您的限额将在 2026-10-20 05:25:22 重置。"}}`))
	}))
	defer server.Close()
	messagesEndpoint = server.URL
	t.Cleanup(func() { messagesEndpoint = defaultMessagesEndpoint })

	raw, _ := json.Marshal(map[string]any{"OriginalRequest": []byte(`{}`), "StorageJSON": []byte(`{"api_key":"k"}`)})
	var env envelope
	_ = json.Unmarshal(execute(raw), &env)
	if env.OK || env.Error.HTTPStatus != 429 || !strings.Contains(env.Error.Message, "1308") {
		t.Fatalf("want 429 with zhipu body, got %+v", env.Error)
	}
}

func TestForEachEventKeepsBytesAndSplitsOnBlankLines(t *testing.T) {
	sse := "event: message_start\ndata: {\"a\":1}\n\nevent: ping\r\ndata: {}\r\n\r\ndata: tail-without-blank"
	var events []string
	if err := forEachEvent(strings.NewReader(sse), func(e []byte) error { events = append(events, string(e)); return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(events, "") != sse || len(events) != 3 {
		t.Fatalf("events %q", events)
	}
}
