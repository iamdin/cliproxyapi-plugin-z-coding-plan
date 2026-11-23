package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultModelsEndpoint = "https://open.bigmodel.cn/api/coding/paas/v4/models"
	modelsFetchTimeout    = 10 * time.Second
	modelsCacheTTL        = 5 * time.Minute
	modelsMaxBodyBytes    = 1 << 20
)

type cachedModels struct {
	ids       []string
	expiresAt time.Time
}

var (
	defaultFallbackModels = []string{"glm-5.3", "glm-5.3-flash"}
	modelsEndpoint        = defaultModelsEndpoint // plugins.configs.z-coding-plan.models-endpoint
	fallbackModels        = append([]string(nil), defaultFallbackModels...)
	modelsClient          = &http.Client{Timeout: modelsFetchTimeout}
	modelsCache           = map[string]cachedModels{}
	modelsCacheGen        uint64
)

func authModels(request []byte) map[string]any {
	key, authID, ok := extractAuthKey(request)

	mu.Lock()
	endpoint := modelsEndpoint
	fallback := append([]string(nil), fallbackModels...)
	gen := modelsCacheGen
	var cacheKey string
	var cached []string
	if ok {
		cacheKey = modelsCacheKey(endpoint, key)
		if entry, found := modelsCache[cacheKey]; found && time.Now().Before(entry.expiresAt) {
			cached = append([]string(nil), entry.ids...)
		}
	}
	mu.Unlock()

	if len(cached) > 0 {
		return modelListResponse(cached)
	}

	if !ok {
		hostLog("warn", fmt.Sprintf("zcp models fetch skipped auth=%s reason=no-api-key; using fallback (%d models)", firstText(authID, "-"), len(fallback)))
		return modelListResponse(fallback)
	}

	ids, err := fetchUpstreamModels(endpoint, key)
	if err != nil {
		hostLog("warn", fmt.Sprintf("zcp models fetch failed auth=%s err=%s; using fallback (%d models)", firstText(authID, "-"), err.Error(), len(fallback)))
		return modelListResponse(fallback)
	}

	mu.Lock()
	if modelsCacheGen == gen {
		modelsCache[cacheKey] = cachedModels{
			ids:       append([]string(nil), ids...),
			expiresAt: time.Now().Add(modelsCacheTTL),
		}
	}
	mu.Unlock()

	return modelListResponse(ids)
}

func modelListResponse(ids []string) map[string]any {
	models := make([]map[string]any, len(ids))
	for i, id := range ids {
		// UserDefined: the host passes thinking settings through instead of judging them.
		models[i] = map[string]any{"ID": id, "Object": "model", "OwnedBy": "zhipu", "UserDefined": true}
	}
	return map[string]any{"Provider": "z-coding-plan", "Models": models}
}

func fetchUpstreamModels(endpoint, apiKey string) ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := modelsClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, modelsMaxBodyBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("models http %d", resp.StatusCode)
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("invalid models json: %w", err)
	}

	seen := map[string]bool{}
	var ids []string
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, errors.New("empty models list")
	}
	return ids, nil
}

func parseModelList(value string) []string {
	seen := map[string]bool{}
	var out []string
	for _, part := range strings.Split(value, ",") {
		id := strings.TrimSpace(part)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func modelsCacheKey(endpoint, apiKey string) string {
	sum := sha256.Sum256([]byte(endpoint + "\x00" + apiKey))
	return hex.EncodeToString(sum[:])
}

func extractAuthKey(raw []byte) (string, string, bool) {
	if len(raw) == 0 {
		return "", "", false
	}
	type nestedAuth struct {
		ID           string          `json:"ID"`
		IDSnake      string          `json:"id"`
		StorageJSON  json.RawMessage `json:"StorageJSON"`
		StorageSnake json.RawMessage `json:"storage_json"`
	}
	var req struct {
		AuthID       string          `json:"AuthID"`
		AuthIDSnake  string          `json:"auth_id"`
		StorageJSON  json.RawMessage `json:"StorageJSON"`
		StorageSnake json.RawMessage `json:"storage_json"`
		Auth         *nestedAuth     `json:"Auth"`
		AuthSnake    *nestedAuth     `json:"auth"`
	}
	if json.Unmarshal(raw, &req) != nil {
		return "", "", false
	}
	authID := firstText(req.AuthID, req.AuthIDSnake)
	if authID == "" && req.Auth != nil {
		authID = firstText(req.Auth.ID, req.Auth.IDSnake)
	}
	if authID == "" && req.AuthSnake != nil {
		authID = firstText(req.AuthSnake.ID, req.AuthSnake.IDSnake)
	}
	candidates := []json.RawMessage{req.StorageJSON, req.StorageSnake}
	if req.Auth != nil {
		candidates = append(candidates, req.Auth.StorageJSON, req.Auth.StorageSnake)
	}
	if req.AuthSnake != nil {
		candidates = append(candidates, req.AuthSnake.StorageJSON, req.AuthSnake.StorageSnake)
	}
	for _, c := range candidates {
		if key, ok := storageKey(c); ok {
			return key, authID, true
		}
	}
	return "", authID, false
}

func storageKey(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", false
	}
	if trimmed[0] == '{' {
		return credentialKey(trimmed)
	}
	if trimmed[0] == '"' {
		var decoded []byte
		if json.Unmarshal(trimmed, &decoded) == nil {
			if key, ok := credentialKey(decoded); ok {
				return key, true
			}
		}
		var text string
		if json.Unmarshal(trimmed, &text) == nil {
			if key, ok := credentialKey([]byte(text)); ok {
				return key, true
			}
		}
	}
	return "", false
}
