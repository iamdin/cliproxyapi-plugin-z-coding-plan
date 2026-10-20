package main

import (
	"strings"
	"time"
)

// No throttling in the gateway: a 1302 goes straight back to the client, which owns queueing and
// retries (CPA's request-scoped-errors rule for 1302 is "stop", so the host does not retry either).
// The plugin only decides which key a request uses, and keeps each session on one key so
// Zhipu's per-account prompt cache stays warm. A session leaves its key only when 1308 blocks it.
const (
	leaseTTL  = 5 * time.Minute // an in-flight mark whose usage record never arrives drops itself
	stickyTTL = time.Hour
	stickyMax = 10000
)

var inflight = map[string][]time.Time{} // per key; only used to spread new sessions

func active(id string, now time.Time) int {
	live := inflight[id][:0]
	for _, at := range inflight[id] {
		if now.Sub(at) < leaseTTL {
			live = append(live, at)
		}
	}
	inflight[id] = live
	return len(live)
}

func release(id string) {
	if len(inflight[id]) > 0 {
		inflight[id] = inflight[id][1:]
	}
}

// pick returns the session's bound key while it is any open (not 1308-blocked) candidate, even
// one outside the top priority tier, so a session never moves just because a key came back.
// Otherwise it takes the least busy top-tier key (first wins ties) and binds the session to it.
// Callers hold mu.
func pick(ids, open []string, session string, now time.Time) string {
	id := boundKey(session, now)
	if !contains(open, id) {
		id = ids[0]
		for _, c := range ids[1:] {
			if active(c, now) < active(id, now) {
				id = c
			}
		}
	}
	inflight[id] = append(inflight[id], now)
	bind(session, id, now)
	return id
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
