package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// quota-warmup: at each configured Beijing time, start every Zhipu key's 5-hour window
// with a 1-token request, so windows reset at predictable times. A 13:00 anchor puts the
// whole 14:00-18:00 double-cost period inside one window that resets at 18:00.
// A key whose current window ends within warmupWaitLimit is pinged right after that reset;
// a key with a longer-running window is left alone.
const (
	warmupEndpoint  = "https://open.bigmodel.cn/api/anthropic/v1/messages"
	warmupWaitLimit = time.Hour
	warmupResetPad  = 3 * time.Second
)

var (
	beijing = time.FixedZone("CST", 8*3600)

	warmupTimes []int // minutes after midnight, Beijing time; plugins.configs.z-coding-plan.quota-warmup-times
	warmupModel = defaultWarmupModel
	warmupStop  chan struct{}
	warmupWG    sync.WaitGroup
	warmupMu    sync.Mutex // guards warmupStop; separate from mu so scheduling never waits on a warmup run
)

const defaultWarmupModel = "glm-5.3"

// parseTimes turns "03:00, 8:00,13:00" into sorted unique minutes; bad entries are skipped.
func parseTimes(value string) []int {
	seen := map[int]bool{}
	var out []int
	for _, part := range strings.Split(value, ",") {
		var h, m int
		if _, err := fmt.Sscanf(strings.TrimSpace(part), "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			continue
		}
		if minute := h*60 + m; !seen[minute] {
			seen[minute] = true
			out = append(out, minute)
		}
	}
	sort.Ints(out)
	return out
}

func nextRun(times []int, now time.Time) time.Time {
	local := now.In(beijing)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, beijing)
	for day := 0; day < 2; day++ {
		for _, minute := range times {
			if at := midnight.AddDate(0, 0, day).Add(time.Duration(minute) * time.Minute); at.After(now) {
				return at
			}
		}
	}
	return time.Time{}
}

// restartWarmup stops any running loop and starts one for the current config.
func restartWarmup() {
	stopWarmup()
	mu.Lock()
	times, model := warmupTimes, warmupModel
	mu.Unlock()
	if len(times) == 0 {
		return
	}
	stop := make(chan struct{})
	warmupMu.Lock()
	warmupStop = stop
	warmupMu.Unlock()
	warmupWG.Add(1)
	go func() {
		defer warmupWG.Done()
		hostLog("info", fmt.Sprintf("quota-warmup: scheduled %s, next %s", formatTimes(times), nextRun(times, time.Now()).Format("01-02 15:04")))
		for {
			timer := time.NewTimer(time.Until(nextRun(times, time.Now())))
			select {
			case <-stop:
				timer.Stop()
				return
			case <-timer.C:
				warmAll(model, stop)
			}
		}
	}()
}

// stopWarmup ends the loop and waits for in-flight pings, so nothing calls the host after shutdown.
func stopWarmup() {
	warmupMu.Lock()
	if warmupStop != nil {
		close(warmupStop)
		warmupStop = nil
	}
	warmupMu.Unlock()
	warmupWG.Wait()
}

func warmAll(model string, stop chan struct{}) {
	keys, err := warmupKeys()
	if err != nil {
		hostLog("warn", "quota-warmup: list keys: "+err.Error())
		return
	}
	var wg sync.WaitGroup
	for label, key := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hostLog("info", "quota-warmup: "+label+": "+warmKey(key, model, stop))
		}()
	}
	wg.Wait()
}

func warmKey(key, model string, stop chan struct{}) string {
	reset, err := fiveHourReset(key)
	if err != nil {
		return "quota check failed: " + err.Error()
	}
	if left := time.Until(reset); left > warmupWaitLimit {
		return "window active until " + reset.In(beijing).Format("15:04:05") + ", skipped"
	} else if left > 0 {
		timer := time.NewTimer(left + warmupResetPad)
		select {
		case <-stop:
			timer.Stop()
			return "stopped while waiting for reset"
		case <-timer.C:
		}
	}
	body, _ := json.Marshal(map[string]any{
		"model": model, "max_tokens": 1,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	status, resp, err := httpDo("POST", warmupEndpoint, map[string][]string{
		"Content-Type":      {"application/json"},
		"Anthropic-Version": {"2023-06-01"},
		"X-Api-Key":         {key},
		"Authorization":     {"Bearer " + key},
	}, body)
	if err != nil {
		return "ping failed: " + err.Error()
	}
	if status < 200 || status >= 300 {
		return fmt.Sprintf("ping http %d: %.200s", status, resp)
	}
	if next, err := fiveHourReset(key); err == nil && !next.IsZero() {
		return "window started, resets " + next.In(beijing).Format("15:04:05")
	}
	return "window started"
}

func fiveHourReset(key string) (time.Time, error) {
	_, windows, err := quotaWindows(key)
	if err != nil {
		return time.Time{}, err
	}
	for _, w := range windows {
		if w.FiveHour && w.HasReset {
			return w.Reset, nil
		}
	}
	return time.Time{}, nil
}

// warmupKeys returns label -> api key for every enabled z-coding-plan auth file.
func warmupKeys() (map[string]string, error) {
	var list struct {
		Files []struct {
			AuthIndex string `json:"auth_index"`
			Name      string `json:"name"`
			Label     string `json:"label"`
			Type      string `json:"type"`
			Provider  string `json:"provider"`
			Disabled  bool   `json:"disabled"`
		} `json:"files"`
	}
	if err := hostResult("host.auth.list", map[string]any{}, &list); err != nil {
		return nil, err
	}
	keys := map[string]string{}
	for _, f := range list.Files {
		if f.Disabled || f.AuthIndex == "" || (f.Provider != "z-coding-plan" && f.Type != "z-coding-plan") {
			continue
		}
		var got struct {
			JSON json.RawMessage `json:"json"`
		}
		if hostResult("host.auth.get", map[string]string{"auth_index": f.AuthIndex}, &got) != nil {
			continue
		}
		if key, ok := credentialKey(got.JSON); ok {
			keys[firstText(f.Label, f.Name, f.AuthIndex)] = key
		}
	}
	return keys, nil
}

func hostResult(method string, request any, out any) error {
	body, _ := json.Marshal(request)
	raw, err := callHost(method, body)
	if err != nil {
		return err
	}
	var env envelope
	if json.Unmarshal(raw, &env) != nil || !env.OK {
		return fmt.Errorf("%s failed", method)
	}
	return json.Unmarshal(env.Result, out)
}

func hostLog(level, message string) {
	body, _ := json.Marshal(map[string]string{"level": level, "message": message})
	_, _ = callHost("host.log", body)
}

func formatTimes(times []int) string {
	parts := make([]string, len(times))
	for i, minute := range times {
		parts[i] = fmt.Sprintf("%02d:%02d", minute/60, minute%60)
	}
	return strings.Join(parts, ",")
}
