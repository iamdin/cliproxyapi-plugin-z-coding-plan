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

func TestSlotsCapConcurrencyPerKey(t *testing.T) {
	resetState(t)
	now := time.Unix(1_800_000_000, 0)
	var got []string
	for {
		id, ok := takeSlot([]string{"a", "b"}, now)
		if !ok {
			break
		}
		got = append(got, id)
	}
	if len(got) != 2*int(minLimit) {
		t.Fatalf("want %v slots per key, got %v", minLimit, got)
	}
	release("a")
	if id, ok := takeSlot([]string{"a", "b"}, now); !ok || id != "a" {
		t.Fatalf("released slot not reused: %s %v", id, ok)
	}
	if _, ok := takeSlot([]string{"a"}, now.Add(leaseTTL)); !ok {
		t.Fatal("stale leases should expire")
	}
}

func Test1302HalvesOncePerBurstAndBacksOff(t *testing.T) {
	resetState(t)
	now := time.Unix(1_800_000_000, 0)
	state("a").limit = 32
	throttled("a", now)
	throttled("a", now) // concurrent 1302 in the same burst
	k := keys["a"]
	if k.limit != 16 || k.level != 1 {
		t.Fatalf("limit %v level %d", k.limit, k.level)
	}
	if _, ok := takeSlot([]string{"a"}, now); ok {
		t.Fatal("paused key handed out")
	}
	first := k.pausedUntil.Sub(now)
	throttled("a", k.pausedUntil)
	if k.limit != minLimit || k.pausedUntil.Sub(now)-first < basePause/2 {
		t.Fatalf("second burst should back off further: limit %v", k.limit)
	}
	for i := 0; i < 20; i++ {
		throttled("a", k.pausedUntil)
	}
	if k.limit != minLimit || k.pausedUntil.Sub(now) <= 0 {
		t.Fatal("limit floor")
	}
	succeeded("a", k.pausedUntil)
	if k.level != 0 || k.limit <= minLimit {
		t.Fatalf("success should recover: level %d limit %v", k.level, k.limit)
	}
}

func TestNoteUsageRoutesErrorCodes(t *testing.T) {
	resetState(t)
	send := func(failed bool, body string) {
		raw, _ := json.Marshal(map[string]any{"AuthID": "a", "Failed": failed, "Failure": map[string]any{"Body": body}})
		noteUsage(raw)
	}
	send(true, `{"code":"1302","message":"[1302][您的账户已达到速率限制]"}`)
	if !keys["a"].pausedUntil.After(time.Now()) {
		t.Fatal("1302 should pause")
	}
	send(true, "[1308][已达到 5 小时的使用上限。您的限额将在 2099-01-01 08:00:00 重置。]")
	if !blocked["a"].After(time.Now()) {
		t.Fatal("1308 should block")
	}
}

func TestScheduleWaitsForFreeSlot(t *testing.T) {
	resetState(t)
	raw, _ := json.Marshal(map[string]any{"Candidates": []map[string]any{
		{"ID": "a", "Attributes": map[string]string{"base_url": "https://open.bigmodel.cn/api/anthropic"}},
	}})
	for i := 0; i < int(minLimit); i++ {
		if got := schedule(raw); got["AuthID"] != "a" {
			t.Fatal(got)
		}
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		noteUsage([]byte(`{"AuthID":"a"}`))
	}()
	start := time.Now()
	if got := schedule(raw); got["AuthID"] != "a" || time.Since(start) < 250*time.Millisecond {
		t.Fatalf("request over the limit should wait for a slot: %v after %v", got, time.Since(start))
	}
	other, _ := json.Marshal(map[string]any{"Candidates": []map[string]any{{"ID": "x"}}})
	if got := schedule(other); got["Handled"] != false {
		t.Fatal("non-Zhipu candidates must stay with the host")
	}
}

func TestSchedulePrefersPriorityUntil1308(t *testing.T) {
	resetState(t)
	raw, _ := json.Marshal(map[string]any{"Candidates": []map[string]any{
		{"ID": "low", "Provider": "z-coding-plan"},
		{"ID": "high", "Provider": "z-coding-plan", "Priority": 10},
	}})
	if got := schedule(raw); got["AuthID"] != "high" {
		t.Fatalf("higher priority key first: %v", got)
	}
	noteUsage([]byte(`{"AuthID":"high","Failed":true,"Failure":{"Body":"[1308][已达到 5 小时的使用上限。您的限额将在 2099-01-01 08:00:00 重置。]"}}`))
	for i := 0; i < 3; i++ {
		if got := schedule(raw); got["AuthID"] != "low" {
			t.Fatalf("1308 key must be skipped on later requests: %v", got)
		}
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
