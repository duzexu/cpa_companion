package companion

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
)

// routeAffinity is local to this companion instance. CPA has its own session
// cache; this cache only chooses which explicit account prefix to send to CPA.
type routeAffinity struct {
	mu         sync.Mutex
	ttl        time.Duration
	bindings   map[string]routeBinding
	lastPicked map[string]string
}

type routeBinding struct {
	accountID string
	expires   time.Time
}

func newRouteAffinity(ttl time.Duration) *routeAffinity {
	return &routeAffinity{ttl: ttl, bindings: make(map[string]routeBinding), lastPicked: make(map[string]string)}
}

type routeSession struct{ ID, Parent string }

func validSessionID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 256 {
		return ""
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return ""
		}
	}
	return raw
}

func jsonObject(raw []byte) map[string]json.RawMessage {
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(raw, &obj)
	return obj
}

func jsonString(obj map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		var value string
		if json.Unmarshal(obj[key], &value) == nil {
			if value = validSessionID(value); value != "" {
				return value
			}
		}
	}
	return ""
}

func nestedObject(obj map[string]json.RawMessage, key string) map[string]json.RawMessage {
	return jsonObject(obj[key])
}

// Session identities follow CPA's explicit-ID precedence for Codex, Claude
// Code and compatible clients. Requests without an explicit identity rotate.
func extractRouteSession(h http.Header, body []byte) routeSession {
	root := jsonObject(body)
	request := nestedObject(root, "request")
	if len(request) == 0 {
		request = root
	}
	meta := nestedObject(request, "metadata")
	if len(meta) == 0 {
		meta = nestedObject(root, "metadata")
	}
	parent := jsonString(request, "parent_session_id", "parentSessionId", "parent_thread_id", "parentThreadId", "forked_from_thread_id", "parent_task_id", "parent_id")
	if parent == "" {
		parent = jsonString(meta, "parent_session_id", "parent_thread_id", "forked_from_thread_id", "parent_task_id", "parent_id")
	}
	if parent == "" {
		parent = validSessionID(h.Get("X-Parent-Session-ID"))
	}
	if parent == "" {
		parent = validSessionID(h.Get("X-Parent-ID"))
	}

	if sid := validSessionID(h.Get("X-Claude-Code-Session-Id")); sid != "" {
		agent := validSessionID(h.Get("X-Claude-Code-Agent-Id"))
		if agent == "" {
			agent = jsonString(meta, "agent_id", "subagent_id")
		}
		if agent != "" && agent != "main" {
			parentAgent := validSessionID(h.Get("X-Claude-Code-Parent-Agent-Id"))
			if parentAgent == "" {
				parentAgent = jsonString(meta, "parent_agent_id")
			}
			if parentAgent != "" && parentAgent != "main" && parentAgent != agent {
				parent = sid + ":agent:" + parentAgent
			} else if parent == "" {
				parent = sid
			}
			return routeSession{"claude:" + sid + ":agent:" + agent, "claude:" + parent}
		}
		if parent == sid {
			parent = ""
		}
		if parent != "" {
			parent = "claude:" + parent
		}
		return routeSession{"claude:" + sid, parent}
	}
	if userID := jsonString(meta, "user_id"); userID != "" {
		if strings.HasPrefix(userID, "{") {
			identity := jsonObject([]byte(userID))
			if sid := jsonString(identity, "session_id"); sid != "" {
				agent := jsonString(identity, "agent_id", "subagent_id")
				if agent != "" && agent != "main" {
					return routeSession{"claude:" + sid + ":agent:" + agent, "claude:" + sid}
				}
				return routeSession{ID: "claude:" + sid}
			}
		}
		if i := strings.LastIndex(userID, "_session_"); i >= 0 {
			if sid := validSessionID(userID[i+9:]); sid != "" {
				return routeSession{ID: "claude:" + sid}
			}
		}
	}
	turn := jsonObject([]byte(h.Get("X-Codex-Turn-Metadata")))
	sid := validSessionID(h.Get("Session-Id"))
	if sid == "" {
		sid = jsonString(turn, "session_id")
	}
	tid := validSessionID(h.Get("Thread-Id"))
	if tid == "" {
		tid = jsonString(turn, "thread_id")
	}
	if tid == "" && sid != "" {
		tid = jsonString(request, "thread_id", "threadId")
	}
	if sid != "" || tid != "" {
		parentThread := validSessionID(h.Get("X-Codex-Parent-Thread-Id"))
		if parentThread == "" {
			parentThread = jsonString(turn, "parent_thread_id")
		}
		fork := jsonString(turn, "forked_from_thread_id", "forked_from_id")
		if fork == "" {
			fork = jsonString(request, "forked_from_thread_id", "forked_from_id")
		}
		child := tid
		if child == "" {
			child = sid
		}
		agent := jsonString(turn, "agent_name")
		agent = strings.TrimPrefix(strings.TrimPrefix(agent, "/root/"), "/")
		if agent != "" && agent != "root" && agent != "main" && sid != "" {
			if parentThread == "" {
				parentThread = sid
			}
			child = sid + ":agent:" + agent
		}
		if fork != "" {
			return routeSession{"codex:" + child, "codex:" + fork}
		}
		if parentThread == "" && tid != "" && sid != "" && tid != sid {
			parentThread = sid
		}
		if parentThread == "" && parent != "" && parent != child {
			parentThread = parent
		}
		if parentThread != "" && parentThread != child {
			return routeSession{"codex:" + child, "codex:" + parentThread}
		}
		return routeSession{ID: "codex:" + child}
	}
	for _, item := range []struct{ header, prefix string }{
		{"X-Http-Session-Id", "agy:"}, {"X-Session-ID", "header:"},
		{"X-Session-Affinity", "affinity:"}, {"X-Slot-Session-Id", "slot:"},
		{"X-Conversation-Id", "conv:"}, {"X-Thread-Id", "thread:"},
		{"X-Client-Request-Id", "clientreq:"},
	} {
		if id := validSessionID(h.Get(item.header)); id != "" {
			p := ""
			if parent != "" && parent != id {
				p = item.prefix + parent
			}
			return routeSession{item.prefix + id, p}
		}
	}
	for _, item := range []struct{ key, prefix string }{
		{"thread_id", "thread:"}, {"session_id", "session:"}, {"sessionId", "session:"},
		{"prompt_cache_key", "pck:"}, {"conversation_id", "conv:"}, {"chat_id", "conv:"},
		{"task_id", "task:"},
	} {
		if id := jsonString(request, item.key); id != "" {
			p := ""
			if parent != "" && parent != id {
				p = item.prefix + parent
			}
			return routeSession{item.prefix + id, p}
		}
	}
	if conv := nestedObject(request, "conversation"); len(conv) > 0 {
		if id := jsonString(conv, "id"); id != "" {
			return routeSession{ID: "conv:" + id}
		}
	}
	return routeSession{}
}

func affinityNamespace(userID string, plan routingPlan, accounts []credential, model string) string {
	// A changed account set or priority changes the namespace immediately.
	h := sha256.New()
	for _, a := range accounts {
		h.Write([]byte(a.ID + "\x00" + a.Prefix + "\x00" + a.PriorityText() + "\x00"))
	}
	return userID + "\x00" + plan.ID + "\x00" + model + "\x00" + hex.EncodeToString(h.Sum(nil))
}

// order chooses a sticky account, otherwise rotates within the highest
// priority tier. Failover candidates retain priority order.
func (a *routeAffinity) order(namespace string, session routeSession, candidates []credential) []credential {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if len(a.bindings) > 10000 {
		for key, value := range a.bindings {
			if !now.Before(value.expires) {
				delete(a.bindings, key)
			}
		}
		for len(a.bindings) > 10000 {
			for key := range a.bindings {
				delete(a.bindings, key)
				break
			}
		}
	}
	key := namespace + "\x00" + session.ID
	parentKey := namespace + "\x00" + session.Parent
	bound := ""
	if session.ID != "" {
		if entry, ok := a.bindings[key]; ok && now.Before(entry.expires) {
			bound = entry.accountID
		}
		if bound == "" && session.Parent != "" {
			if entry, ok := a.bindings[parentKey]; ok && now.Before(entry.expires) {
				bound = entry.accountID
			}
		}
	}
	start := -1
	for i := range candidates {
		if candidates[i].ID == bound {
			start = i
			break
		}
	}
	if start < 0 {
		last := a.lastPicked[namespace]
		top := 1
		for top < len(candidates) && samePriority(candidates[0], candidates[top]) {
			top++
		}
		start = 0
		for i := 0; i < top; i++ {
			if candidates[i].ID == last {
				start = (i + 1) % top
				break
			}
		}
		a.lastPicked[namespace] = candidates[start].ID
	}
	ordered := make([]credential, 0, len(candidates))
	ordered = append(ordered, candidates[start])
	for i := range candidates {
		if i != start {
			ordered = append(ordered, candidates[i])
		}
	}
	if session.ID != "" {
		a.bindings[key] = routeBinding{ordered[0].ID, now.Add(a.ttl)}
	}
	return ordered
}

func samePriority(a, b credential) bool {
	if a.Priority == nil || b.Priority == nil {
		return a.Priority == nil && b.Priority == nil
	}
	return *a.Priority == *b.Priority
}

func (a *routeAffinity) rebind(namespace, sessionID, previous, next string) {
	if sessionID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	key := namespace + "\x00" + sessionID
	if current, ok := a.bindings[key]; ok && current.accountID == previous {
		if next == "" {
			delete(a.bindings, key)
		} else {
			a.bindings[key] = routeBinding{next, time.Now().Add(a.ttl)}
		}
	}
}
