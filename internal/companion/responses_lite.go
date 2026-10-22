package companion

import (
	"encoding/json"
	"net/http"
	"strings"
)

const responsesLiteHeader = "X-OpenAI-Internal-Codex-Responses-Lite"
const responsesLiteMetadata = "ws_request_header_x_openai_internal_codex_responses_lite"

// CPA recognizes both the HTTP header and the WebSocket metadata mirror as
// Responses Lite. The upstream requires all_turns whenever either is true.
func ensureResponsesLiteContext(header http.Header, payload map[string]json.RawMessage, body []byte) ([]byte, error) {
	if !strings.EqualFold(strings.TrimSpace(header.Get(responsesLiteHeader)), "true") && !responsesLiteInMetadata(payload["client_metadata"]) {
		return body, nil
	}

	var reasoning map[string]json.RawMessage
	if raw := payload["reasoning"]; len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &reasoning); err != nil || reasoning == nil {
			// Leave malformed reasoning to CPA's usual request validation.
			return body, nil
		}
	}
	if reasoning == nil {
		reasoning = make(map[string]json.RawMessage)
	}
	var context string
	if json.Unmarshal(reasoning["context"], &context) == nil && context == "all_turns" {
		return body, nil
	}
	reasoning["context"] = json.RawMessage(`"all_turns"`)
	encoded, err := json.Marshal(reasoning)
	if err != nil {
		return nil, err
	}
	payload["reasoning"] = encoded
	return json.Marshal(payload)
}

func responsesLiteInMetadata(raw json.RawMessage) bool {
	var metadata map[string]json.RawMessage
	if json.Unmarshal(raw, &metadata) != nil {
		return false
	}
	value := metadata[responsesLiteMetadata]
	var enabled bool
	if json.Unmarshal(value, &enabled) == nil {
		return enabled
	}
	var text string
	return json.Unmarshal(value, &text) == nil && strings.EqualFold(strings.TrimSpace(text), "true")
}
