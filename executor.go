package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Passthrough executor: glm requests routed to z-coding-plan credentials go to Zhipu exactly as the
// client sent them, Anthropic Messages to the Anthropic endpoint and OpenAI chat completions to the
// Coding Plan OpenAI endpoint. Only the key changes.
// CPA's built-in claude executor rewrites Messages requests (Claude Code cloaking, thinking
// stripping, tool renaming, ?beta=true), so a call through CPA no longer matches a direct call.
const (
	defaultMessagesEndpoint = "https://open.bigmodel.cn/api/anthropic/v1/messages"
	defaultChatEndpoint     = "https://open.bigmodel.cn/api/coding/paas/v4/chat/completions"
)

var (
	messagesEndpoint  = defaultMessagesEndpoint // plugins.configs.z-coding-plan.messages-endpoint
	chatEndpoint      = defaultChatEndpoint     // plugins.configs.z-coding-plan.chat-completions-endpoint
	passthroughModels = []string{"glm-5.3", "glm-5.3-flash"}
	upstreamClient    = &http.Client{} // no timeout: thinking streams run for minutes
)

// Client headers that belong to the hop to CPA (or carry CPA's own credentials) and must not reach Zhipu.
// accept-encoding is left to Go's transport, which then hands back plain bytes.
var hopHeaders = map[string]bool{
	"authorization": true, "x-api-key": true, "cookie": true, "host": true, "content-length": true,
	"connection": true, "keep-alive": true, "proxy-connection": true, "proxy-authorization": true,
	"te": true, "trailer": true, "transfer-encoding": true, "upgrade": true, "accept-encoding": true,
	"via": true, "forwarded": true, "x-forwarded-for": true, "x-forwarded-host": true,
	"x-forwarded-proto": true, "x-real-ip": true,
}

type executorRequest struct {
	Headers         http.Header
	OriginalRequest []byte
	StorageJSON     []byte
	SourceFormat    string // "openai" for /v1/chat/completions, "claude" for /v1/messages
	StreamID        string `json:"stream_id"`
}

func authModels() map[string]any {
	models := make([]map[string]any, len(passthroughModels))
	for i, id := range passthroughModels {
		// UserDefined: the host passes thinking settings through instead of judging them.
		models[i] = map[string]any{"ID": id, "Object": "model", "OwnedBy": "zhipu", "UserDefined": true}
	}
	return map[string]any{"Provider": "z-coding-plan", "Models": models}
}

// callUpstream sends the client's original body to Zhipu. On failure it returns an error
// envelope carrying Zhipu's status and body, so 1302/1308 handling and host retries still work.
func callUpstream(raw []byte) (*http.Response, executorRequest, []byte) {
	var req executorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, req, errorEnvelope("executor_error", "bad executor request: "+err.Error())
	}
	key, ok := credentialKey(req.StorageJSON)
	if !ok {
		return nil, req, errorEnvelope("executor_error", "z-coding-plan credential has no api_key")
	}
	if len(req.OriginalRequest) == 0 {
		return nil, req, errorEnvelope("executor_error", "empty request body")
	}
	mu.Lock()
	endpoint := messagesEndpoint
	if req.SourceFormat == "openai" {
		endpoint = chatEndpoint
	}
	mu.Unlock()
	httpReq, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(req.OriginalRequest))
	if err != nil {
		return nil, req, errorEnvelope("executor_error", err.Error())
	}
	for name, values := range req.Headers {
		if !hopHeaders[strings.ToLower(name)] {
			httpReq.Header[name] = values
		}
	}
	if req.SourceFormat == "openai" {
		httpReq.Header.Set("Authorization", "Bearer "+key)
	} else {
		httpReq.Header.Set("X-Api-Key", key)
	}
	resp, err := upstreamClient.Do(httpReq)
	if err != nil {
		return nil, req, errorEnvelope("upstream_error", err.Error(), http.StatusBadGateway)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, req, errorEnvelope("upstream_error", string(body), resp.StatusCode)
	}
	return resp, req, nil
}

func responseHeaders(resp *http.Response) http.Header {
	out := http.Header{}
	for name, values := range resp.Header {
		if !hopHeaders[strings.ToLower(name)] && !strings.EqualFold(name, "Content-Encoding") {
			out[name] = values
		}
	}
	return out
}

func execute(raw []byte) []byte {
	resp, _, failed := callUpstream(raw)
	if failed != nil {
		return failed
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return errorEnvelope("upstream_error", err.Error(), http.StatusBadGateway)
	}
	return okResult(map[string]any{"Payload": body, "Headers": responseHeaders(resp)})
}

// executeStream answers once Zhipu has accepted the request, then relays the SSE body
// event by event through host.stream.emit.
func executeStream(raw []byte) []byte {
	resp, req, failed := callUpstream(raw)
	if failed != nil {
		return failed
	}
	if req.StreamID == "" {
		resp.Body.Close()
		return errorEnvelope("executor_error", "stream_id is required")
	}
	go relay(resp.Body, req.StreamID, req.SourceFormat == "openai")
	return okResult(map[string]any{"headers": responseHeaders(resp)})
}

// relay emits Anthropic SSE events byte for byte. For OpenAI streams the host writes the SSE framing
// itself ("data: <chunk>\n\n" plus its own [DONE]), so only each event's data goes through.
func relay(body io.ReadCloser, streamID string, openai bool) {
	defer body.Close()
	err := forEachEvent(body, func(event []byte) error {
		if openai {
			if event = sseData(event); event == nil {
				return nil
			}
		}
		return streamCall("host.stream.emit", streamID, event, "")
	})
	message := ""
	if err != nil {
		message = err.Error()
	}
	_ = streamCall("host.stream.close", streamID, nil, message)
}

// forEachEvent splits an SSE body into events (each ending with its blank line) without altering bytes.
func forEachEvent(r io.Reader, emit func([]byte) error) error {
	reader := bufio.NewReaderSize(r, 64<<10)
	var event []byte
	for {
		line, err := reader.ReadBytes('\n')
		event = append(event, line...)
		blank := len(line) > 0 && len(bytes.TrimRight(line, "\r\n")) == 0
		if len(event) > 0 && (blank || err != nil) {
			if errEmit := emit(event); errEmit != nil {
				return errEmit
			}
			event = nil
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// sseData returns an event's data payload (multiple data lines joined by newlines), or nil for
// events without data and for the [DONE] marker.
func sseData(event []byte) []byte {
	var data [][]byte
	for _, line := range bytes.Split(event, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if rest, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data = append(data, bytes.TrimPrefix(rest, []byte(" ")))
		}
	}
	joined := bytes.Join(data, []byte("\n"))
	if len(data) == 0 || string(joined) == "[DONE]" {
		return nil
	}
	return joined
}

func streamCall(method, streamID string, payload []byte, message string) error {
	body, _ := json.Marshal(map[string]any{"stream_id": streamID, "payload": payload, "error": message})
	raw, err := callHost(method, body)
	if err != nil {
		return err
	}
	var env envelope
	if json.Unmarshal(raw, &env) != nil || !env.OK {
		return fmt.Errorf("%s rejected", method)
	}
	return nil
}
