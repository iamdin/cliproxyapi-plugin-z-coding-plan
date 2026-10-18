package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestResetAtUsesChinaTime(t *testing.T) {
	got := resetAt("code 1308 reset 2026-10-18 23:29:07", time.Unix(0, 0))
	want := time.Date(2026, 10, 18, 23, 29, 7, 0, time.FixedZone("CST", 8*3600))
	if !got.Equal(want) {
		t.Fatal(got)
	}
	if !resetAt("other error", time.Unix(100, 0)).IsZero() {
		t.Fatal("non-1308")
	}
	if !resetAt("upstream request 130812 timed out", time.Unix(100, 0)).IsZero() {
		t.Fatal("embedded digits")
	}
	if !resetAt("code 1308", time.Unix(100, 0)).IsZero() {
		t.Fatal("missing reset time")
	}
}

func TestDecideLeavesHostInChargeUntilBlocked(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	raw, _ := json.Marshal(map[string]any{
		"Candidates": []map[string]string{{"ID": "a"}, {"ID": "b"}},
	})
	if _, handled := decide(raw, now, map[string]time.Time{}); handled {
		t.Fatal("handled while healthy")
	}
	id, handled := decide(raw, now, map[string]time.Time{"a": now.Add(time.Hour)})
	if !handled || id != "b" {
		t.Fatalf("%s %v", id, handled)
	}
	id, handled = decide(raw, now, map[string]time.Time{
		"a": now.Add(2 * time.Hour),
		"b": now.Add(time.Hour),
	})
	if !handled || id != "b" {
		t.Fatalf("earliest %s %v", id, handled)
	}
}

func TestMock1308SkipsThatKeyUntilReset(t *testing.T) {
	mu.Lock()
	blocked = map[string]time.Time{}
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		blocked = map[string]time.Time{}
		mu.Unlock()
	})
	usage, _ := json.Marshal(map[string]any{
		"AuthID": "key-a",
		"Failure": map[string]any{
			"Body": "code 1308 reset 2099-01-01 08:00:00",
		},
	})
	noteUsage(usage)
	raw, _ := json.Marshal(map[string]any{
		"Candidates": []map[string]string{{"ID": "key-a"}, {"ID": "key-b"}},
	})
	got := schedule(raw)
	if got["Handled"] != true || got["AuthID"] != "key-b" {
		t.Fatalf("%v", got)
	}
	other, _ := json.Marshal(map[string]any{"AuthID": "key-b", "Failure": map[string]any{"Body": "temporary"}})
	noteUsage(other)
	if again := schedule(raw); again["AuthID"] != "key-b" {
		t.Fatalf("%v", again)
	}
}

func TestCredentialKeyAndEndpoint(t *testing.T) {
	stored, _ := json.Marshal(map[string]string{"api_key": "secret-value"})
	key, ok := credentialKey(stored)
	if !ok || key != "secret-value" {
		t.Fatalf("%q %v", key, ok)
	}
	got := endpointFromConfig("enabled: true" + string(rune(10)) + "endpoint: \"https://example.test/quota\"")
	if got != "https://example.test/quota" {
		t.Fatal(got)
	}
	if endpointFromConfig("") != defaultEndpoint {
		t.Fatal("default")
	}
	q := string(rune(34))
	commented := "endpoint: " + q + "https://example.test/quota" + q + " # old"
	if endpointFromConfig(commented) != "https://example.test/quota" {
		t.Fatal(endpointFromConfig(commented))
	}
	if endpointFromConfig("endpoint: "+q+"http://collector.example/quota"+q) != defaultEndpoint {
		t.Fatal("http")
	}
}
