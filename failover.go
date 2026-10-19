package main

import (
	"encoding/json"
	"time"
)

func resetAt(message string, now time.Time) time.Time {
	if !code1308(message) {
		return time.Time{}
	}
	zone := time.FixedZone("CST", 8*3600)
	for i := 0; i+19 <= len(message); i++ {
		parsed, err := time.ParseInLocation("2006-01-02 15:04:05", message[i:i+19], zone)
		if err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func code1308(message string) bool { return hasCode(message, "1308") }

// hasCode reports whether code appears in message as a standalone number.
func hasCode(message, code string) bool {
	for i := 0; i+4 <= len(message); i++ {
		if message[i:i+4] != code {
			continue
		}
		if i > 0 && message[i-1] >= '0' && message[i-1] <= '9' {
			continue
		}
		if i+4 < len(message) && message[i+4] >= '0' && message[i+4] <= '9' {
			continue
		}
		return true
	}
	return false
}

func pickIndex(until []time.Time, now time.Time) int {
	var eligible []int
	for i, item := range until {
		if !item.After(now) {
			eligible = append(eligible, i)
		}
	}
	if len(eligible) == 0 {
		if len(until) == 0 {
			return -1
		}
		best := 0
		for i := 1; i < len(until); i++ {
			if until[i].Before(until[best]) {
				best = i
			}
		}
		return best
	}
	return eligible[0]
}

func decide(request []byte, now time.Time, blocked map[string]time.Time) (string, bool) {
	var req struct {
		Candidates []struct {
			ID string `json:"ID"`
		} `json:"Candidates"`
	}
	if json.Unmarshal(request, &req) != nil || len(req.Candidates) == 0 {
		return "", false
	}
	until := make([]time.Time, len(req.Candidates))
	blockedAny := false
	for i, candidate := range req.Candidates {
		deadline := blocked[candidate.ID]
		until[i] = deadline
		if deadline.After(now) {
			blockedAny = true
		}
	}
	if !blockedAny {
		return "", false
	}
	index := pickIndex(until, now)
	if index < 0 || index >= len(req.Candidates) {
		return "", false
	}
	return req.Candidates[index].ID, true
}
