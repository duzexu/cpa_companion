package companion

import (
	"bytes"
	"encoding/json"
)

// imageUsageScanner finds a top-level usage object in an image JSON response
// or SSE data frame without retaining the (often multi-megabyte) base64 image.
// The response is still forwarded unchanged by usageCapture.
type imageUsageScanner struct {
	depth        int
	inString     bool
	escaped      bool
	expectKey    bool
	readingKey   bool
	key          []byte
	usageKey     bool
	awaitValue   bool
	captureDepth int
	capture      []byte
	oversized    bool
	pending      tokenUsage
	pendingValid bool
}

const maxImageUsageObject = 64 << 10

func (s *imageUsageScanner) write(chunk []byte) (tokenUsage, bool) {
	var latest tokenUsage
	found := false
	for _, b := range chunk {
		if s.captureDepth > 0 && !s.oversized {
			if len(s.capture) < maxImageUsageObject {
				s.capture = append(s.capture, b)
			} else {
				s.capture = nil
				s.oversized = true
			}
		}
		if s.inString {
			switch {
			case s.escaped:
				s.escaped = false
			case b == '\\':
				s.escaped = true
			case b == '"':
				s.inString = false
				if s.readingKey {
					s.usageKey = bytes.Equal(s.key, []byte("usage"))
					s.readingKey = false
					s.expectKey = false
				}
			default:
				if s.readingKey && len(s.key) < 16 {
					s.key = append(s.key, b)
				}
			}
			continue
		}
		if b == '"' {
			s.inString = true
			if s.depth == 1 && s.expectKey {
				s.readingKey = true
				s.key = s.key[:0]
			}
			if s.awaitValue {
				s.awaitValue = false
			}
			continue
		}
		if s.awaitValue && b != ' ' && b != '\t' && b != '\r' && b != '\n' {
			s.awaitValue = false
			if b == '{' && s.depth == 1 {
				s.captureDepth = s.depth
				s.capture = append(s.capture[:0], b)
				s.oversized = false
			}
		}
		switch b {
		case '{', '[':
			s.depth++
			if s.depth == 1 {
				s.expectKey = true
			}
		case '}', ']':
			if s.depth > 0 {
				s.depth--
			}
			if s.captureDepth > 0 && s.depth == s.captureDepth {
				if !s.oversized {
					var fields map[string]json.RawMessage
					if json.Unmarshal(s.capture, &fields) == nil {
						wrapped, _ := json.Marshal(map[string]any{"usage": fields})
						if usage, ok := parseResponseUsage("/v1/images/generations", wrapped); ok {
							s.pending, s.pendingValid = usage, true
						}
					}
				}
				s.captureDepth, s.capture, s.oversized = 0, nil, false
			}
			if s.depth == 0 {
				if s.pendingValid {
					latest, found = s.pending, true
				}
				s.pending, s.pendingValid = tokenUsage{}, false
				s.expectKey, s.usageKey, s.awaitValue = false, false, false
			}
		case ':':
			if s.depth == 1 && s.usageKey {
				s.awaitValue = true
			}
		case ',':
			if s.depth == 1 {
				s.expectKey, s.usageKey, s.awaitValue = true, false, false
			}
		}
	}
	return latest, found
}
