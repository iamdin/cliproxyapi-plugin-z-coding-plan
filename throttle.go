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
	stickyWait      = 10 * time.Second // how long a session waits for its own key before moving to another
	stickyTTL       = time.Hour
	stickyMax       = 10000
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

// usable reports whether a key can take traffic at all right now (not 1308-blocked, not paused).
func usable(id string, now time.Time) bool {
	return !blocked[id].After(now) && !state(id).pausedUntil.After(now)
}

// takeSlot reserves a slot, preferring `prefer` (the session's bound key, keeps the prompt cache warm)
// and otherwise the open key with the most headroom. With holdForPrefer, a usable-but-full
// preferred key is waited for (returns false) instead of moving the session to another key.
func takeSlot(ids []string, now time.Time, prefer string, holdForPrefer bool) (string, bool) {
	free := func(id string) float64 { return state(id).limit - float64(state(id).active(now)) }
	if prefer != "" && usable(prefer, now) && contains(ids, prefer) {
		if free(prefer) >= 1 {
			keys[prefer].inflight = append(keys[prefer].inflight, now)
			return prefer, true
		}
		if holdForPrefer {
			return "", false
		}
	}
	best, room := "", 0.0
	for _, id := range ids {
		if usable(id, now) && free(id) >= 1 && free(id) > room {
			best, room = id, free(id)
		}
	}
	if best == "" {
		return "", false
	}
	keys[best].inflight = append(keys[best].inflight, now)
	return best, true
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
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
		if c.Provider != "z-coding-plan" && !strings.Contains(c.Attributes["base_url"], "bigmodel.cn") {
			return false
		}
	}
	return len(candidates) > 0
}

// Session -> key binding, so consecutive requests of one conversation hit the same Zhipu account
// (prompt cache is per account). In-memory only.
type binding struct {
	id   string
	seen time.Time
}

var sticky = map[string]binding{}

// sessionKey picks the best available session identity from the scheduler request.
// Subagents use their parent's session so they share its key and cache.
// ponytail: explicit session signals only (CPA metadata, Claude Code/Codex headers); requests with none are not sticky.
func sessionKey(headers map[string][]string, metadata map[string]any) string {
	for _, k := range []string{"parent_session_id", "canonical_session_id", "derived_session_id", "execution_session_id"} {
		if v, _ := metadata[k].(string); strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	for _, want := range []string{"x-claude-code-session-id", "session-id", "session_id", "x-session-id", "x-session-affinity"} {
		for name, values := range headers {
			if strings.EqualFold(name, want) && len(values) > 0 && strings.TrimSpace(values[0]) != "" {
				return strings.TrimSpace(values[0])
			}
		}
	}
	return ""
}

func boundKey(session string, now time.Time) string {
	b, ok := sticky[session]
	if !ok || now.Sub(b.seen) > stickyTTL {
		return ""
	}
	return b.id
}

func bind(session, id string, now time.Time) {
	if session == "" {
		return
	}
	if len(sticky) >= stickyMax {
		for k, b := range sticky {
			if now.Sub(b.seen) > stickyTTL {
				delete(sticky, k)
			}
		}
		if len(sticky) >= stickyMax {
			sticky = map[string]binding{}
		}
	}
	sticky[session] = binding{id, now}
}
