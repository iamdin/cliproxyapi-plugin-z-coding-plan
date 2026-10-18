package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

const defaultEndpoint = "https://bigmodel.cn/api/monitor/usage/quota/limit"

var (
	mu       sync.Mutex
	endpoint = defaultEndpoint
	blocked  = map[string]time.Time{}
)

func dispatch(method string, request []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		configure(request)
		return okResult(registration()), nil
	case "plugin.quiesce", "plugin.shutdown":
		clearKeys()
		return okResult(map[string]any{}), nil
	case "auth.identifier", "quota.identifier":
		return okResult(map[string]string{"identifier": "z-coding-plan"}), nil
	case "auth.parse":
		return okResult(parseAuth(request)), nil
	case "auth.login.start", "auth.login.poll", "auth.refresh":
		return okResult(map[string]any{}), nil
	case "quota.describe":
		return okResult(map[string]any{
			"supported_providers": []string{"z-coding-plan"},
			"display_name":        "智谱 Coding Plan",
			"supports_reset":      false,
		}), nil
	case "quota.fetch":
		result, err := fetchQuota(request)
		if err != nil {
			return nil, err
		}
		return okResult(result), nil
	case "quota.reset":
		return okResult(map[string]any{"success": false, "message": "智谱额度不能从这里重置"}), nil
	case "scheduler.pick":
		return okResult(schedule(request)), nil
	case "usage.handle":
		noteUsage(request)
		return okResult(map[string]any{}), nil
	default:
		return nil, fmt.Errorf("unknown method %s", method)
	}
}

func registration() map[string]any {
	return map[string]any{
		"schema_version": 6,
		"metadata": map[string]any{
			"Name":             "Z Coding Plan",
			"Version":          "0.1.0",
			"Author":           "oxwen11",
			"GitHubRepository": "https://github.com/oxwen11/cliproxyapi-plugin-z-coding-plan",
			"ConfigFields": []map[string]any{{
				"Name":        "endpoint",
				"Type":        "string",
				"Default":     defaultEndpoint,
				"Description": "Zhipu quota URL. The default is the Coding Plan quota endpoint.",
			}},
		},
		"capabilities": map[string]any{
			"auth_provider":  true,
			"quota_provider": true,
			"scheduler":      true,
			"usage_plugin":   true,
		},
	}
}

func configure(request []byte) {
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	_ = json.Unmarshal(request, &req)
	next := endpointFromConfig(string(req.ConfigYAML))
	mu.Lock()
	endpoint = next
	mu.Unlock()
}

func endpointFromConfig(yaml string) string {
	for _, line := range strings.Split(yaml, string(rune(10))) {
		text := strings.TrimSpace(strings.TrimRight(line, string(rune(13))))
		if text == "" || strings.HasPrefix(text, "#") || !strings.HasPrefix(text, "endpoint:") {
			continue
		}
		value := strings.Trim(strings.TrimSpace(strings.TrimPrefix(text, "endpoint:")), "\"'")
		if value != "" {
			return value
		}
	}
	return defaultEndpoint
}

func parseAuth(request []byte) map[string]any {
	var req struct {
		FileName string `json:"FileName"`
		RawJSON  []byte `json:"RawJSON"`
	}
	if json.Unmarshal(request, &req) != nil {
		return map[string]any{"Handled": false}
	}
	var file struct {
		Type   string `json:"type"`
		APIKey string `json:"api_key"`
		Label  string `json:"label"`
	}
	if json.Unmarshal(req.RawJSON, &file) != nil || file.Type != "z-coding-plan" || strings.TrimSpace(file.APIKey) == "" {
		return map[string]any{"Handled": false}
	}
	if file.Label == "" {
		file.Label = strings.TrimSuffix(req.FileName, ".json")
	}
	id := strings.TrimSuffix(req.FileName, ".json")
	if id == "" {
		id = file.Label
	}
	return map[string]any{
		"Handled": true,
		"Auth": map[string]any{
			"Provider":    "z-coding-plan",
			"ID":          id,
			"FileName":    req.FileName,
			"Label":       file.Label,
			"StorageJSON": req.RawJSON,
		},
	}
}

func fetchQuota(request []byte) (map[string]any, error) {
	var req struct {
		StorageJSON []byte `json:"storage_json"`
	}
	if json.Unmarshal(request, &req) != nil {
		return nil, fmt.Errorf("quota credential not found")
	}
	key, ok := credentialKey(req.StorageJSON)
	if !ok {
		return nil, fmt.Errorf("quota credential not found")
	}
	mu.Lock()
	quotaURL := endpoint
	mu.Unlock()
	plan, buckets, err := queryQuota(quotaURL, key)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"subscription": map[string]string{"plan": plan},
		"groups": []map[string]any{{
			"displayName": "智谱 Coding Plan",
			"buckets":     buckets,
		}},
	}, nil
}

func credentialKey(storage []byte) (string, bool) {
	var body struct {
		APIKey      string `json:"api_key"`
		APIKeyAlias string `json:"api-key"`
	}
	if json.Unmarshal(storage, &body) != nil {
		return "", false
	}
	key := strings.TrimSpace(body.APIKey)
	if key == "" {
		key = strings.TrimSpace(body.APIKeyAlias)
	}
	if key == "" {
		return "", false
	}
	return key, true
}

func queryQuota(quotaURL, apiKey string) (string, []map[string]any, error) {
	request, _ := json.Marshal(map[string]any{
		"Method": "GET",
		"URL":    quotaURL,
		"Headers": map[string][]string{
			"Authorization": {apiKey},
			"Accept":        {"application/json"},
		},
	})
	raw, err := callHost("host.http.do", request)
	if err != nil {
		return "", nil, err
	}
	var env envelope
	if json.Unmarshal(raw, &env) != nil || !env.OK {
		return "", nil, fmt.Errorf("quota request failed")
	}
	var response struct {
		StatusCode int    `json:"StatusCode"`
		Body       []byte `json:"Body"`
	}
	if json.Unmarshal(env.Result, &response) != nil {
		return "", nil, fmt.Errorf("quota response unreadable")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", nil, fmt.Errorf("quota http %d", response.StatusCode)
	}
	plan, windows, err := parseZhipuQuota(response.Body, time.Now())
	if err != nil {
		return "", nil, err
	}
	buckets := make([]map[string]any, 0, len(windows))
	for _, window := range windows {
		item := map[string]any{
			"window":            window.Label,
			"remainingFraction": (100 - window.Used) / 100,
			"description":       window.Label,
		}
		if window.HasReset {
			item["resetTime"] = window.Reset.UTC().Format(time.RFC3339)
		}
		buckets = append(buckets, item)
	}
	return plan, buckets, nil
}

func schedule(request []byte) map[string]any {
	mu.Lock()
	defer mu.Unlock()
	id, handled := decide(request, time.Now(), blocked)
	if !handled {
		return map[string]any{"Handled": false}
	}
	return map[string]any{"Handled": true, "AuthID": id}
}

func noteUsage(request []byte) {
	var req struct {
		AuthID  string `json:"AuthID"`
		Failure struct {
			Body string `json:"Body"`
		} `json:"Failure"`
	}
	if json.Unmarshal(request, &req) != nil || req.AuthID == "" || !strings.Contains(req.Failure.Body, "1308") {
		return
	}
	until := resetAt(req.Failure.Body, time.Now())
	if until.IsZero() || !until.After(time.Now()) {
		return
	}
	mu.Lock()
	blocked[req.AuthID] = until
	mu.Unlock()
}

func clearKeys() {
	mu.Lock()
	defer mu.Unlock()
	endpoint = defaultEndpoint
	blocked = map[string]time.Time{}
}

func okResult(value any) []byte {
	raw, _ := json.Marshal(value)
	out, _ := json.Marshal(envelope{OK: true, Result: raw})
	return out
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
