package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

type quotaWindow struct {
	Label    string
	Used     float64
	Reset    time.Time
	HasReset bool
}

func parseZhipuQuota(body []byte, now time.Time) (string, []quotaWindow, error) {
	var payload struct {
		Code    *int   `json:"code"`
		Success *bool  `json:"success"`
		Msg     string `json:"msg"`
		Data    struct {
			PlanName    string          `json:"planName"`
			Plan        string          `json:"plan"`
			PlanType    string          `json:"plan_type"`
			PackageName string          `json:"packageName"`
			Level       string          `json:"level"`
			Limits      json.RawMessage `json:"limits"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &payload) != nil || len(payload.Data.Limits) == 0 || string(payload.Data.Limits) == "null" {
		return "", nil, fmt.Errorf("quota response has no windows")
	}
	if payload.Success != nil && !*payload.Success {
		return "", nil, fmt.Errorf("quota request rejected")
	}
	if payload.Code != nil && *payload.Code != 200 {
		return "", nil, fmt.Errorf("quota request rejected")
	}
	var rawLimits []map[string]any
	if json.Unmarshal(payload.Data.Limits, &rawLimits) != nil {
		return "", nil, fmt.Errorf("quota response has no windows")
	}
	var windows []quotaWindow
	for _, raw := range rawLimits {
		kind, _ := raw["type"].(string)
		if kind != "TOKENS_LIMIT" && kind != "TIME_LIMIT" && kind != "CREDIT_LIMIT" {
			continue
		}
		unit, unitOK := whole(raw["unit"])
		number, numberOK := whole(raw["number"])
		percent, percentOK := whole(raw["percentage"])
		if !unitOK || !numberOK || !percentOK {
			return "", nil, fmt.Errorf("quota window is incomplete")
		}
		minutes := windowMinutes(kind, unit, number)
		used := float64(percent)
		if usage, ok := whole(raw["usage"]); ok && usage > 0 {
			remaining, hasRemaining := whole(raw["remaining"])
			current, hasCurrent := whole(raw["currentValue"])
			consumed, known := 0, false
			if hasRemaining {
				consumed = usage - remaining
				if hasCurrent && current > consumed {
					consumed = current
				}
				known = true
			} else if hasCurrent {
				consumed = current
				known = true
			}
			if known {
				if consumed < 0 {
					consumed = 0
				}
				if consumed > usage {
					consumed = usage
				}
				used = float64(consumed) * 100 / float64(usage)
			}
		}
		if used < 0 {
			used = 0
		}
		if used > 100 {
			used = 100
		}
		item := quotaWindow{Label: windowName(kind, unit, number, minutes), Used: used}
		if reset, ok := whole(raw["nextResetTime"]); ok && reset > 0 && resetIsUsable(kind, minutes, int64(reset), now) {
			item.Reset = time.UnixMilli(int64(reset)).UTC()
			item.HasReset = true
		}
		windows = append(windows, item)
	}
	if len(windows) == 0 {
		return "", nil, fmt.Errorf("quota response has no windows")
	}
	plan := firstText(payload.Data.PlanName, payload.Data.Plan, payload.Data.PlanType, payload.Data.PackageName, payload.Data.Level)
	return plan, windows, nil
}

func whole(value any) (int, bool) {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number != math.Trunc(number) {
		return 0, false
	}
	return int(number), true
}

func windowMinutes(kind string, unit, number int) int {
	if kind == "TIME_LIMIT" && unit == 5 && number == 1 {
		return 30 * 24 * 60
	}
	multipliers := map[int]int{1: 1440, 3: 60, 5: 1, 6: 10080}
	if number <= 0 || multipliers[unit] == 0 {
		return 0
	}
	return number * multipliers[unit]
}

func windowName(kind string, unit, number, minutes int) string {
	if kind == "TIME_LIMIT" {
		return "MCP"
	}
	prefix := "额度"
	if kind == "CREDIT_LIMIT" {
		prefix = "积分"
	}
	switch {
	case minutes == 300:
		return prefix + " 5小时"
	case unit == 3 && number > 0:
		return fmt.Sprintf("%s %d小时", prefix, number)
	case unit == 1 && number > 0:
		return fmt.Sprintf("%s %d天", prefix, number)
	case unit == 6 && number > 0:
		return fmt.Sprintf("%s %d周", prefix, number)
	default:
		return prefix
	}
}

func resetIsUsable(kind string, minutes int, reset int64, now time.Time) bool {
	if kind == "TIME_LIMIT" || minutes != 300 {
		return true
	}
	return reset <= now.Add(5*time.Hour+time.Minute).UnixMilli()
}

func firstText(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
