package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

const quotaEndpoint = "https://bigmodel.cn/api/monitor/usage/quota/limit"

var (
	mu      sync.Mutex
	blocked = map[string]time.Time{}
)

func dispatch(method string, request []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		configure(request)
		return okResult(registration()), nil
	case "plugin.quiesce", "plugin.shutdown":
		stopWarmup()
		clearKeys()
		return okResult(map[string]any{}), nil
	case "auth.identifier", "quota.identifier", "executor.identifier":
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
	case "model.static":
		return okResult(map[string]any{"Provider": "z-coding-plan"}), nil
	case "model.for_auth":
		return okResult(authModels()), nil
	case "executor.execute":
		return execute(request), nil
	case "executor.execute_stream":
		return executeStream(request), nil
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
			"Author":           "iamdin",
			"GitHubRepository": "https://github.com/iamdin/cliproxyapi-plugin-z-coding-plan",
			"ConfigFields": []map[string]any{
				{"Name": "min-concurrency", "Type": "integer", "Default": 8, "Description": "每把智谱 Key 的起始和最低并发数"},
				{"Name": "max-concurrency", "Type": "integer", "Default": 64, "Description": "每把智谱 Key 的最高并发数"},
				{"Name": "quota-warmup-times", "Type": "string", "Default": "", "Description": "北京时间，逗号分隔，如 03:00,08:00,13:00,18:00,23:00；到点用 1 token 请求开启每把 Key 的 5 小时窗口，留空关闭"},
				{"Name": "quota-warmup-model", "Type": "string", "Default": defaultWarmupModel, "Description": "quota-warmup 请求使用的模型"},
				{"Name": "messages-endpoint", "Type": "string", "Default": defaultMessagesEndpoint, "Description": "原样转发的智谱 Anthropic Messages 地址（核对转发时可指向本地抓包服务）"},
			},
		},
		"capabilities": map[string]any{
			"auth_provider":               true,
			"quota_provider":              true,
			"scheduler":                   true,
			"scheduler_across_priorities": true,
			"usage_plugin":                true,
			"model_provider":              true,
			"executor":                    true,
			"executor_model_scope":        "both",
			"executor_input_formats":      []string{"claude"},
			"executor_output_formats":     []string{"claude"},
		},
	}
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
	plan, buckets, err := queryQuota(key)
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

func queryQuota(apiKey string) (string, []map[string]any, error) {
	plan, windows, err := quotaWindows(apiKey)
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

type candidate struct {
	ID         string            `json:"ID"`
	Provider   string            `json:"Provider"`
	Priority   int               `json:"Priority"`
	Attributes map[string]string `json:"Attributes"`
}

// topTier keeps the highest-priority candidates whose key is not blocked by 1308. The host hands
// us every priority tier (scheduler_across_priorities), so a blocked high-priority key no longer
// shadows the lower tier. Callers hold mu.
func topTier(candidates []candidate, now time.Time) []string {
	best, ids := 0, []string(nil)
	for _, c := range candidates {
		if blocked[c.ID].After(now) {
			continue
		}
		if ids == nil || c.Priority > best {
			best, ids = c.Priority, nil
		}
		if c.Priority == best {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

func schedule(request []byte) map[string]any {
	var req struct {
		Candidates []candidate `json:"Candidates"`
	}
	_ = json.Unmarshal(request, &req)
	// ponytail: polls every 200ms instead of a wake-up queue; waiters are not FIFO.
	for start := time.Now(); zhipuOnly(req.Candidates); time.Sleep(pollEvery) {
		mu.Lock()
		now := time.Now()
		ids := topTier(req.Candidates, now)
		if len(ids) == 0 || now.Sub(start) > maxWait {
			mu.Unlock()
			break
		}
		id, ok := takeSlot(ids, now)
		mu.Unlock()
		if ok {
			return map[string]any{"Handled": true, "AuthID": id}
		}
	}
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
		Failed  bool   `json:"Failed"`
		Failure struct {
			Body string `json:"Body"`
		} `json:"Failure"`
	}
	if json.Unmarshal(request, &req) != nil || req.AuthID == "" {
		return
	}
	now := time.Now()
	mu.Lock()
	defer mu.Unlock()
	release(req.AuthID)
	switch until := resetAt(req.Failure.Body, now); {
	case until.After(now):
		blocked[req.AuthID] = until
	case hasCode(req.Failure.Body, "1302"):
		throttled(req.AuthID, now)
	case !req.Failed:
		succeeded(req.AuthID, now)
	}
}

func clearKeys() {
	mu.Lock()
	defer mu.Unlock()
	blocked = map[string]time.Time{}
	keys = map[string]*keyState{}
}

func okResult(value any) []byte {
	raw, _ := json.Marshal(value)
	out, _ := json.Marshal(envelope{OK: true, Result: raw})
	return out
}

func errorEnvelope(code, message string, httpStatus ...int) []byte {
	e := &envelopeError{Code: code, Message: message}
	if len(httpStatus) > 0 {
		e.HTTPStatus = httpStatus[0]
	}
	raw, _ := json.Marshal(envelope{OK: false, Error: e})
	return raw
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func quotaWindows(apiKey string) (string, []quotaWindow, error) {
	status, body, err := httpDo("GET", quotaEndpoint, map[string][]string{
		"Authorization": {apiKey},
		"Accept":        {"application/json"},
	}, nil)
	if err != nil {
		return "", nil, err
	}
	if status < 200 || status >= 300 {
		return "", nil, fmt.Errorf("quota http %d", status)
	}
	return parseZhipuQuota(body, time.Now())
}

func httpDo(method, url string, headers map[string][]string, body []byte) (int, []byte, error) {
	request, _ := json.Marshal(map[string]any{"Method": method, "URL": url, "Headers": headers, "Body": body})
	raw, err := callHost("host.http.do", request)
	if err != nil {
		return 0, nil, err
	}
	var env envelope
	if json.Unmarshal(raw, &env) != nil || !env.OK {
		return 0, nil, fmt.Errorf("%s %s failed", method, url)
	}
	var response struct {
		StatusCode int    `json:"StatusCode"`
		Body       []byte `json:"Body"`
	}
	if json.Unmarshal(env.Result, &response) != nil {
		return 0, nil, fmt.Errorf("%s %s response unreadable", method, url)
	}
	return response.StatusCode, response.Body, nil
}

// configure reads the plugin's config YAML: min-concurrency / max-concurrency (see throttle.go)
// and quota-warmup-times / quota-warmup-model (see warmup.go).
// ponytail: flat "key: value" lines only; swap in a YAML parser if nested config appears.
func configure(request []byte) {
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	_ = json.Unmarshal(request, &req)
	lo, hi := defaultMinLimit, defaultMaxLimit
	var times []int
	model := defaultWarmupModel
	endpoint := defaultMessagesEndpoint
	for _, line := range strings.Split(string(req.ConfigYAML), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		n, err := strconv.ParseFloat(value, 64)
		switch strings.TrimSpace(key) {
		case "min-concurrency":
			if err == nil && n >= 1 {
				lo = math.Floor(n)
			}
		case "max-concurrency":
			if err == nil && n >= 1 {
				hi = math.Floor(n)
			}
		case "quota-warmup-times":
			times = parseTimes(value)
		case "quota-warmup-model":
			if value != "" {
				model = value
			}
		case "messages-endpoint":
			if value != "" {
				endpoint = value
			}
		}
	}
	mu.Lock()
	minLimit, maxLimit = lo, max(lo, hi)
	for _, k := range keys {
		k.limit = min(maxLimit, max(minLimit, k.limit))
	}
	warmupTimes, warmupModel = times, model
	messagesEndpoint = endpoint
	mu.Unlock()
	restartWarmup()
}
