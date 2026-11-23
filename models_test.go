package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

func resetModelsConfig(t *testing.T) {
	t.Helper()
	configure(nil)
	t.Cleanup(func() {
		configure(nil)
		clearKeys()
	})
}

func modelIDsFromResult(t *testing.T, res map[string]any) []string {
	t.Helper()
	if res["Provider"] != "z-coding-plan" {
		t.Fatalf("unexpected provider: %v", res["Provider"])
	}
	rawModels, ok := res["Models"].([]map[string]any)
	if !ok {
		t.Fatalf("unexpected Models type: %T", res["Models"])
	}
	ids := make([]string, len(rawModels))
	for i, m := range rawModels {
		if m["Object"] != "model" || m["OwnedBy"] != "zhipu" || m["UserDefined"] != true {
			t.Fatalf("unexpected model entry %d: %+v", i, m)
		}
		ids[i], _ = m["ID"].(string)
	}
	return ids
}

func TestAuthModelsDynamicFetchAndCache(t *testing.T) {
	resetModelsConfig(t)

	var hits atomic.Int32
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"object": "list",
			"data": [
				{"id": " glm-5.3 "},
				{"id": ""},
				{"id": "   "},
				{"id": "glm-4.7"},
				{"id": "glm-5.3"},
				{"id": "glm-5.3-flash"}
			]
		}`))
	}))
	defer server.Close()

	cfg, _ := json.Marshal(map[string]any{
		"config_yaml": []byte("models-endpoint: " + server.URL + "\n"),
	})
	configure(cfg)

	req, _ := json.Marshal(map[string]any{
		"AuthID":      "key-1",
		"StorageJSON": []byte(`{"type":"z-coding-plan","api_key":"zhipu-secret"}`),
	})

	got := modelIDsFromResult(t, authModels(req))
	want := []string{"glm-5.3", "glm-4.7", "glm-5.3-flash"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if gotAuth != "Bearer zhipu-secret" {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, "Bearer zhipu-secret")
	}
	if hits.Load() != 1 {
		t.Fatalf("expected 1 upstream call, got %d", hits.Load())
	}

	// Second call should hit the in-memory cache.
	got2 := modelIDsFromResult(t, authModels(req))
	if !reflect.DeepEqual(got2, want) || hits.Load() != 1 {
		t.Fatalf("expected cached result with 1 hit, got %v hits=%d", got2, hits.Load())
	}

	// clearKeys invalidates the cache.
	clearKeys()
	got3 := modelIDsFromResult(t, authModels(req))
	if !reflect.DeepEqual(got3, want) || hits.Load() != 2 {
		t.Fatalf("expected cache miss after clearKeys, got %v hits=%d", got3, hits.Load())
	}
}

func TestAuthModelsFallbackToConfiguredModels(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		closed  bool
	}{
		{
			name: "http error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"boom"}`))
			},
		},
		{
			name: "invalid json",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`not-json`))
			},
		},
		{
			name: "empty list",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"  "}]}`))
			},
		},
		{
			name:   "network error",
			closed: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetModelsConfig(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.handler != nil {
					tc.handler(w, r)
				}
			}))
			url := server.URL
			if tc.closed {
				server.Close()
			} else {
				defer server.Close()
			}

			cfg, _ := json.Marshal(map[string]any{
				"config_yaml": []byte("models: \"glm-custom-1, glm-custom-2, glm-custom-1\"\nmodels-endpoint: " + url + "\n"),
			})
			configure(cfg)

			req, _ := json.Marshal(map[string]any{
				"AuthID":      "key-1",
				"StorageJSON": []byte(`{"api_key":"k"}`),
			})
			got := modelIDsFromResult(t, authModels(req))
			want := []string{"glm-custom-1", "glm-custom-2"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %v, want %v", got, want)
			}
		})
	}
}

func TestAuthModelsFallbackToDefaultWhenUnconfigured(t *testing.T) {
	resetModelsConfig(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	cfg, _ := json.Marshal(map[string]any{
		"config_yaml": []byte("models-endpoint: " + server.URL + "\n"),
	})
	configure(cfg)

	req, _ := json.Marshal(map[string]any{
		"StorageJSON": []byte(`{"api_key":"k"}`),
	})
	got := modelIDsFromResult(t, authModels(req))
	if !reflect.DeepEqual(got, defaultFallbackModels) {
		t.Fatalf("got %v, want %v", got, defaultFallbackModels)
	}
}

func TestConfigureReadsModelsSettings(t *testing.T) {
	resetModelsConfig(t)

	raw, _ := json.Marshal(map[string]any{
		"config_yaml": []byte("models: \" glm-a, glm-b , , glm-a \"\nmodels-endpoint: \"https://example.test/v4/models\"\n"),
	})
	configure(raw)
	mu.Lock()
	gotModels := append([]string(nil), fallbackModels...)
	gotEndpoint := modelsEndpoint
	mu.Unlock()
	if !reflect.DeepEqual(gotModels, []string{"glm-a", "glm-b"}) {
		t.Fatalf("fallbackModels = %v", gotModels)
	}
	if gotEndpoint != "https://example.test/v4/models" {
		t.Fatalf("modelsEndpoint = %q", gotEndpoint)
	}

	// Omitted settings reset to defaults.
	configure(nil)
	mu.Lock()
	gotModels = append([]string(nil), fallbackModels...)
	gotEndpoint = modelsEndpoint
	mu.Unlock()
	if !reflect.DeepEqual(gotModels, defaultFallbackModels) || gotEndpoint != defaultModelsEndpoint {
		t.Fatalf("reset failed: models=%v endpoint=%q", gotModels, gotEndpoint)
	}
}

func TestExtractAuthKeyPayloads(t *testing.T) {
	pascal, _ := json.Marshal(map[string]any{
		"AuthID":      "a1",
		"StorageJSON": []byte(`{"api_key":"k-pascal"}`),
	})
	snake, _ := json.Marshal(map[string]any{
		"auth_id":      "a2",
		"storage_json": []byte(`{"api-key":"k-snake"}`),
	})
	nestedPascal, _ := json.Marshal(map[string]any{
		"Auth": map[string]any{
			"ID":          "a3",
			"StorageJSON": []byte(`{"api_key":"k-nested"}`),
		},
	})
	nestedSnake := []byte(`{"auth":{"id":"a4","storage_json":{"api_key":"k-obj"}}}`)
	rawString := []byte(`{"StorageJSON":"{\"api_key\":\"k-str\"}"}`)
	missing := []byte(`{"AuthID":"a5"}`)

	cases := []struct {
		name   string
		raw    []byte
		wantK  string
		wantID string
		wantOK bool
	}{
		{"pascal base64", pascal, "k-pascal", "a1", true},
		{"snake base64 alias", snake, "k-snake", "a2", true},
		{"nested pascal", nestedPascal, "k-nested", "a3", true},
		{"nested snake object", nestedSnake, "k-obj", "a4", true},
		{"raw json string", rawString, "k-str", "", true},
		{"missing key", missing, "", "a5", false},
		{"empty body", nil, "", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, id, ok := extractAuthKey(tc.raw)
			if k != tc.wantK || id != tc.wantID || ok != tc.wantOK {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, %v)", k, id, ok, tc.wantK, tc.wantID, tc.wantOK)
			}
		})
	}
}

func TestDispatchModelForAuth(t *testing.T) {
	resetModelsConfig(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"glm-dynamic"}]}`))
	}))
	defer server.Close()

	cfg, _ := json.Marshal(map[string]any{
		"config_yaml": []byte("models-endpoint: " + server.URL + "\n"),
	})
	configure(cfg)

	req, _ := json.Marshal(map[string]any{
		"StorageJSON": []byte(`{"api_key":"k-dispatch"}`),
	})
	raw, err := dispatch("model.for_auth", req)
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("unexpected envelope: %s", raw)
	}
	var res struct {
		Provider string `json:"Provider"`
		Models   []struct {
			ID          string `json:"ID"`
			Object      string `json:"Object"`
			OwnedBy     string `json:"OwnedBy"`
			UserDefined bool   `json:"UserDefined"`
		} `json:"Models"`
	}
	if err := json.Unmarshal(env.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.Provider != "z-coding-plan" || len(res.Models) != 1 || res.Models[0].ID != "glm-dynamic" || !res.Models[0].UserDefined || res.Models[0].OwnedBy != "zhipu" {
		t.Fatalf("unexpected result: %+v", res)
	}
}
