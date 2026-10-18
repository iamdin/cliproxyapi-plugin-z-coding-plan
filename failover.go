package main

import (
	"encoding/json"
	"strings"
	"time"
)

func resetAt(message string, now time.Time) time.Time {
	if !strings.Contains(message, "1308") {
		return time.Time{}
	}
	for i := 0; i+19 <= len(message); i++ {
		stamp := message[i : i+19]
		if !clockStamp(stamp) {
			continue
		}
		parsed, err := time.ParseInLocation("2006-01-02 15:04:05", stamp, time.FixedZone("CST", 8*3600))
		if err == nil {
			return parsed
		}
	}
	return now.Add(5 * time.Hour)
}

func clockStamp(value string) bool {
	for i, ch := range value {
		switch i {
		case 4, 7:
			if ch != '-' {
				return false
			}
		case 10:
			if ch != ' ' {
				return false
			}
		case 13, 16:
			if ch != ':' {
				return false
			}
		default:
			if ch < '0' || ch > '9' {
				return false
			}
		}
	}
	return true
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
