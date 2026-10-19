package main

import (
	"math/rand"
	"strings"
	"time"
)

// Per-key adaptive concurrency (AIMD): each success grows a key's limit a little,
// each 1302 halves it (not below minLimit) and pauses the key with exponential backoff plus jitter.
// Requests over the limit wait in scheduler.pick instead of hitting Zhipu.
// Overridable via plugins.configs.z-coding-plan.min-concurrency / max-concurrency.
var minLimit, maxLimit = defaultMinLimit, defaultMaxLimit // min is also the starting limit

const (
	defaultMinLimit = 8.0
	defaultMaxLimit = 64.0
	basePause       = 2 * time.Second
	maxPause        = 60 * time.Second
	leaseTTL        = 5 * time.Minute // a slot whose usage record never arrives frees itself
	maxWait         = 90 * time.Second
	pollEvery       = 200 * time.Millisecond
)

type keyState struct {
	limit       float64
	inflight    []time.Time
	pausedUntil time.Time
	level       int
}

var keys = map[string]*keyState{}

func state(id string) *keyState {
	k := keys[id]
	if k == nil {
		k = &keyState{limit: minLimit}
		keys[id] = k
	}
	return k
}

func (k *keyState) active(now time.Time) int {
	live := k.inflight[:0]
	for _, at := range k.inflight {
		if now.Sub(at) < leaseTTL {
			live = append(live, at)
		}
	}
	k.inflight = live
	return len(live)
}

// takeSlot reserves a slot on the open key with the most headroom.
func takeSlot(ids []string, now time.Time) (string, bool) {
	best, room := "", 0.0
	for _, id := range ids {
		if blocked[id].After(now) {
			continue
		}
		k := state(id)
		if k.pausedUntil.After(now) {
			continue
		}
		if free := k.limit - float64(k.active(now)); free >= 1 && free > room {
			best, room = id, free
		}
	}
	if best == "" {
		return "", false
	}
	keys[best].inflight = append(keys[best].inflight, now)
	return best, true
}

func release(id string) {
	if k := keys[id]; k != nil && len(k.inflight) > 0 {
		k.inflight = k.inflight[1:]
	}
}

func throttled(id string, now time.Time) {
	k := state(id)
	if k.pausedUntil.After(now) {
		return // same burst: concurrent 1302s count once
	}
	k.limit = max(minLimit, k.limit/2)
	d := min(maxPause, basePause<<min(k.level, 5))
	d = d/2 + time.Duration(rand.Int63n(int64(d/2)+1))
	k.pausedUntil = now.Add(d)
	k.level++
}

func succeeded(id string, now time.Time) {
	k := state(id)
	k.limit = min(maxLimit, k.limit+1/k.limit)
	if !k.pausedUntil.After(now) {
		k.level = 0
	}
}

func zhipuOnly(candidates []candidate) bool {
	for _, c := range candidates {
		if !strings.Contains(c.Attributes["base_url"], "bigmodel.cn") {
			return false
		}
	}
	return len(candidates) > 0
}

func allBlocked(ids []string, now time.Time) bool {
	for _, id := range ids {
		if !blocked[id].After(now) {
			return false
		}
	}
	return true
}
