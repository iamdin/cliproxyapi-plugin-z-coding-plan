package main

import (
	"encoding/json"
	"testing"
	"time"
)

func resetState(t *testing.T) {
	clearKeys()
	t.Cleanup(clearKeys)
}

func scheduleFor(t *testing.T, session string, candidates ...map[string]any) string {
	t.Helper()
	req := map[string]any{"Candidates": candidates}
	if session != "" {
		req["Options"] = map[string]any{"Headers": map[string][]string{"X-Claude-Code-Session-Id": {session}}}
	}
	raw, _ := json.Marshal(req)
	got := schedule(raw)
	if got["Handled"] != true {
		t.Fatalf("not handled: %v", got)
	}
	return got["AuthID"].(string)
}

func zhipu(id string, priority int) map[string]any {
	return map[string]any{"ID": id, "Provider": "z-coding-plan", "Priority": priority}
}

func TestNewSessionsSpreadAndNeverWait(t *testing.T) {
	resetState(t)
	a, b := zhipu("a", 0), zhipu("b", 0)
	var got []string
	for i := 0; i < 4; i++ { // no session: least busy key each time, no limit, no waiting
		got = append(got, scheduleFor(t, "", a, b))
	}
	if got[0] != "a" || got[1] != "b" || got[2] != "a" || got[3] != "b" {
		t.Fatal(got)
	}
	noteUsage([]byte(`{"AuthID":"a"}`))
	noteUsage([]byte(`{"AuthID":"a"}`))
	if id := scheduleFor(t, "", a, b); id != "a" {
		t.Fatalf("finished requests should free the key: %s", id)
	}
	if load("b", time.Now().Add(leaseTTL)) != 0 {
		t.Fatal("lost usage records should expire")
	}
}

func TestTargetsSteerNewSessionsWithoutWaiting(t *testing.T) {
	resetState(t)
	low, high := zhipu("low", 0), zhipu("high", 10)
	n := int(minLimit)
	for i := 0; i < n; i++ {
		if id := scheduleFor(t, "", low, high); id != "high" {
			t.Fatalf("top tier until its target: %d %s", i, id)
		}
	}
	if id := scheduleFor(t, "", low, high); id != "low" {
		t.Fatalf("top tier at its target: overflow to the next tier, got %s", id)
	}
	for i := 1; i < n; i++ {
		scheduleFor(t, "", low, high)
	}
	if id := scheduleFor(t, "", low, high); id != "high" {
		t.Fatalf("everything at target: least loaded top-tier key, no waiting, got %s", id)
	}

	clearKeys()
	a, b := zhipu("a", 0), zhipu("b", 0)
	state("a").limit, state("b").limit = 16, 16
	noteUsage([]byte(`{"AuthID":"a","Failed":true,"Failure":{"Body":"[1302][您的账户已达到速率限制]"}}`))
	noteUsage([]byte(`{"AuthID":"a","Failed":true,"Failure":{"Body":"[1302][您的账户已达到速率限制]"}}`))
	if keys["a"].limit != 8 {
		t.Fatalf("1302 burst halves once: %v", keys["a"].limit)
	}
	got := map[string]int{}
	for i := 0; i < 12; i++ {
		got[scheduleFor(t, "", a, b)]++
	}
	if got["a"] != 4 || got["b"] != 8 {
		t.Fatalf("new work should follow the targets 8:16: %v", got)
	}
	noteUsage([]byte(`{"AuthID":"b"}`))
	if keys["b"].limit <= 16 {
		t.Fatal("success should raise the target")
	}
}

func TestConfigureReadsConcurrencyBounds(t *testing.T) {
	resetState(t)
	t.Cleanup(func() { configure(nil) })
	state("a").limit = 100
	raw, _ := json.Marshal(map[string]any{"config_yaml": []byte("enabled: true\npriority: 1\nmin-concurrency: 4\nmax-concurrency: 32\n")})
	configure(raw)
	if minLimit != 4 || maxLimit != 32 || keys["a"].limit != 32 {
		t.Fatalf("min %v max %v limit %v", minLimit, maxLimit, keys["a"].limit)
	}
	raw, _ = json.Marshal(map[string]any{"config_yaml": []byte("min-concurrency: 16\nmax-concurrency: 2\n")})
	configure(raw)
	if minLimit != 16 || maxLimit != 16 {
		t.Fatalf("max below min: %v %v", minLimit, maxLimit)
	}
	configure([]byte(`{"config_yaml":"ZW5hYmxlZDogdHJ1ZQo="}`))
	if minLimit != defaultMinLimit || maxLimit != defaultMaxLimit {
		t.Fatal("omitted fields should fall back to defaults")
	}
}

func TestSessionStaysOnItsKeyThrough1302(t *testing.T) {
	resetState(t)
	a, b := zhipu("a", 0), zhipu("b", 0)
	first := scheduleFor(t, "s1", a, b)
	for i := 0; i < 10; i++ { // busy key and 1302s do not move the session
		noteUsage([]byte(`{"AuthID":"` + first + `","Failed":true,"Failure":{"Body":"{\"error\":{\"code\":\"1302\",\"message\":\"[1302][您的账户已达到速率限制]\"}}"}}`))
		if id := scheduleFor(t, "s1", a, b); id != first {
			t.Fatalf("session moved to %s", id)
		}
	}
	noteUsage([]byte(`{"AuthID":"` + first + `","Failed":true,"Failure":{"Body":"[1308][已达到 5 小时的使用上限。您的限额将在 2099-01-01 08:00:00 重置。]"}}`))
	if id := scheduleFor(t, "s1", a, b); id == first {
		t.Fatal("1308 must move the session")
	}
}

func TestSessionKeepsLowerTierKeyAfterHighComesBack(t *testing.T) {
	resetState(t)
	low, high := zhipu("low", 0), zhipu("high", 10)
	blocked["high"] = time.Now().Add(time.Hour)
	if id := scheduleFor(t, "s1", low, high); id != "low" {
		t.Fatal(id)
	}
	delete(blocked, "high")
	if id := scheduleFor(t, "s1", low, high); id != "low" {
		t.Fatalf("session should stay on its key: %s", id)
	}
	if id := scheduleFor(t, "s2", low, high); id != "high" {
		t.Fatalf("new sessions use the top tier: %s", id)
	}
}

func TestSchedulePrefersPriorityUntil1308(t *testing.T) {
	resetState(t)
	low, high := zhipu("low", 0), zhipu("high", 10)
	if id := scheduleFor(t, "", low, high); id != "high" {
		t.Fatalf("higher priority key first: %v", id)
	}
	noteUsage([]byte(`{"AuthID":"high","Failed":true,"Failure":{"Body":"[1308][已达到 5 小时的使用上限。您的限额将在 2099-01-01 08:00:00 重置。]"}}`))
	for i := 0; i < 3; i++ {
		if id := scheduleFor(t, "", low, high); id != "low" {
			t.Fatalf("1308 key must be skipped on later requests: %v", id)
		}
	}
	other, _ := json.Marshal(map[string]any{"Candidates": []map[string]any{{"ID": "x"}}})
	if got := schedule(other); got["Handled"] != false {
		t.Fatal("non-Zhipu candidates must stay with the host")
	}
}

func TestSessionKeySources(t *testing.T) {
	if got := sessionKey(map[string][]string{"X-Claude-Code-Session-Id": {"s1"}}, nil); got != "s1" {
		t.Fatal(got)
	}
	if got := sessionKey(nil, map[string]any{"parent_session_id": "p", "canonical_session_id": "c"}); got != "p" {
		t.Fatalf("subagent should follow parent: %s", got)
	}
	now := time.Now()
	bind("s1", "b", now)
	if boundKey("s1", now.Add(stickyTTL+time.Second)) != "" {
		t.Fatal("binding should expire")
	}
}
