package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// No throttling in the gateway: a 1302 goes straight back to the client, which owns queueing and
// retries (CPA's request-scoped-errors rule for 1302 is "stop", so the host does not retry either).
// The plugin only decides which key a request uses:
//   - a session stays on its key so Zhipu's per-account prompt cache stays warm; it moves only when
//     1308 blocks that key;
//   - new sessions go to a key below its target concurrency, highest priority first, least loaded
//     within a tier. If every key is at its target, the least loaded top-tier key takes it anyway.
//
// Targets adapt per key (AIMD): each success adds 1/limit, a 1302 halves it (once per burst, not
// below min-concurrency). Bounds: plugins.configs.z-coding-plan.min-concurrency / max-concurrency.
var minLimit, maxLimit = defaultMinLimit, defaultMaxLimit // min is also the starting target

const (
	defaultMinLimit = 8.0
	defaultMaxLimit = 64.0
	burstWindow     = 2 * time.Second // 1302s within this window of a cut count as the same burst
	leaseTTL        = 5 * time.Minute // an in-flight mark whose usage record never arrives drops itself
	stickyTTL       = time.Hour
	stickyMax       = 10000
)

type keyState struct {
	limit    float64
	inflight []time.Time
	lastCut  time.Time
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

// load is the key's in-flight count over its target; below 1 means it has room.
func load(id string, now time.Time) float64 {
	k := state(id)
	live := k.inflight[:0]
	for _, at := range k.inflight {
		if now.Sub(at) < leaseTTL {
			live = append(live, at)
		}
	}
	k.inflight = live
	return float64(len(live)) / k.limit
}

func release(id string) {
	if k := keys[id]; k != nil && len(k.inflight) > 0 {
		k.inflight = k.inflight[1:]
	}
}

func throttled(id string, now time.Time) {
	k := state(id)
	if now.Sub(k.lastCut) < burstWindow {
		return
	}
	k.limit = max(minLimit, k.limit/2)
	k.lastCut = now
}

func succeeded(id string) {
	k := state(id)
	k.limit = min(maxLimit, k.limit+1/k.limit)
}

// pick chooses a key from tiers (open keys grouped by priority, highest first; never empty),
// marks it in flight and binds the session to it. Callers hold mu.
func pick(tiers [][]string, session string, now time.Time) string {
	id := boundKey(session, now)
	if !inTiers(tiers, id) {
		id = ""
		for _, tier := range tiers {
			if best := leastLoaded(tier, now); load(best, now) < 1 {
				id = best
				break
			}
		}
		if id == "" {
			id = leastLoaded(tiers[0], now)
		}
	}
	k := state(id)
	k.inflight = append(k.inflight, now)
	bind(session, id, now)
	return id
}

func leastLoaded(ids []string, now time.Time) string {
	best := ids[0]
	for _, id := range ids[1:] {
		if load(id, now) < load(best, now) {
			best = id
		}
	}
	return best
}

func inTiers(tiers [][]string, id string) bool {
	for _, tier := range tiers {
		if contains(tier, id) {
			return true
		}
	}
	return false
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func zhipuOnly(candidates []candidate) bool {
	for _, c := range candidates {
		if c.Provider != "z-coding-plan" && !strings.Contains(c.Attributes["base_url"], "bigmodel.cn") {
			return false
		}
	}
	return len(candidates) > 0
}

// Session -> key binding. In-memory only.
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

// sessionHash is how logs show a session: a short stable hash, "-" for none.
func sessionHash(session string) string {
	if session == "" {
		return "-"
	}
	sum := sha256.Sum256([]byte(session))
	return hex.EncodeToString(sum[:4])
}

func boundKey(session string, now time.Time) string {
	b, ok := sticky[session]
	if session == "" || !ok || now.Sub(b.seen) > stickyTTL {
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
