package main

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestParseTimes(t *testing.T) {
	got := parseTimes(" 13:00,03:00, 8:00,bad,25:00,13:00,23:59")
	if want := []int{180, 480, 780, 1439}; !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	if parseTimes("") != nil {
		t.Fatal("empty should disable")
	}
}

func TestNextRunUsesBeijingTime(t *testing.T) {
	times := parseTimes("03:00,13:00,18:00")
	at := func(h, m int) time.Time { return time.Date(2026, 10, 19, h, m, 0, 0, beijing) }
	cases := map[time.Time]time.Time{
		at(12, 59): at(13, 0),
		at(13, 0):  at(18, 0), // exactly on a slot: that run already fired
		at(23, 0):  at(3, 0).AddDate(0, 0, 1),
	}
	for now, want := range cases {
		if got := nextRun(times, now.UTC()); !got.Equal(want) {
			t.Fatalf("%v -> %v, want %v", now, got, want)
		}
	}
}

func TestConfigureReadsWarmupSettings(t *testing.T) {
	t.Cleanup(func() { configure(nil) })
	raw, _ := json.Marshal(map[string]any{"config_yaml": []byte("enabled: true\nquota-warmup-times: \"03:00,13:00\"\nquota-warmup-model: glm-x\n")})
	configure(raw)
	mu.Lock()
	times, model := warmupTimes, warmupModel
	mu.Unlock()
	if !reflect.DeepEqual(times, []int{180, 780}) || model != "glm-x" {
		t.Fatal(times, model)
	}
	warmupMu.Lock()
	running := warmupStop != nil
	warmupMu.Unlock()
	if !running {
		t.Fatal("warmup loop should be running")
	}
	warmupMu.Lock()
	first := warmupStop
	warmupMu.Unlock()
	configure(raw) // host reconfigures with the same settings: keep the loop
	warmupMu.Lock()
	same := warmupStop == first
	warmupMu.Unlock()
	if !same {
		t.Fatal("unchanged config should not restart the loop")
	}
	stopWarmup()
	configure(nil)
	if warmupTimes != nil || warmupModel != defaultWarmupModel || warmupStop != nil {
		t.Fatal("omitted settings should turn warmup off")
	}
}
