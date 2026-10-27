package companion

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/tokenkit"
)

const maxUsageCapture = 8 << 20

// usageCapture forwards bytes without delaying a stream, while retaining only
// the usage-bearing JSON or SSE frames needed for local budget accounting.
type usageCapture struct {
	target     http.ResponseWriter
	route      string
	model      string
	status     int
	stream     bool
	tooLarge   bool
	body       bytes.Buffer
	line       []byte
	eventData  []byte
	image      imageUsageScanner
	usage      tokenUsage
	hasUsage   bool
	finalUsage bool
	written    int64
	// Count generated text in SSE deltas for interrupted streams. Framing,
	// event metadata and keepalives are not model output.
	outputBytes int64
	hasDelta    bool
	outputText  strings.Builder
}

func (w *usageCapture) Header() http.Header { return w.target.Header() }

func (w *usageCapture) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.stream = strings.Contains(strings.ToLower(w.Header().Get("Content-Type")), "text/event-stream")
	w.target.WriteHeader(status)
}

func (w *usageCapture) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.status >= 200 && w.status < 300 {
		if w.route == "/v1/images/generations" {
			if usage, ok := w.image.write(p); ok {
				w.mergeUsage(usage)
			}
		} else if w.stream {
			w.consumeSSE(p)
		} else if !w.tooLarge {
			if w.body.Len()+len(p) > maxUsageCapture {
				w.tooLarge = true
				w.body.Reset()
			} else {
				_, _ = w.body.Write(p)
			}
		}
	}
	n, err := w.target.Write(p)
	w.written += int64(n)
	return n, err
}

func (w *usageCapture) Flush() {
	if flush, ok := w.target.(http.Flusher); ok {
		flush.Flush()
	}
}

func (w *usageCapture) consumeSSE(chunk []byte) {
	for _, b := range chunk {
		if b != '\n' {
			if len(w.line) < 1<<20 {
				w.line = append(w.line, b)
			} else {
				w.tooLarge = true
			}
			continue
		}
		line := bytes.TrimSuffix(w.line, []byte{'\r'})
		if len(line) == 0 {
			w.finishEvent()
		} else if bytes.HasPrefix(line, []byte("data:")) && !w.tooLarge {
			data := bytes.TrimSpace(line[5:])
			if len(w.eventData)+len(data) < 1<<20 {
				w.eventData = append(w.eventData, data...)
			} else {
				w.tooLarge = true
			}
		}
		w.line = w.line[:0]
	}
}

func (w *usageCapture) finishEvent() {
	if len(w.eventData) > 0 {
		w.captureOutputDelta(w.eventData)
		if usage, ok := parseResponseUsage(w.route, w.eventData); ok {
			w.mergeUsage(usage)
			if w.route == "/v1/responses" {
				var event struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(w.eventData, &event) == nil && event.Type == "response.completed" {
					w.finalUsage = true
				}
			}
		}
		w.eventData = w.eventData[:0]
	}
}

// captureOutputDelta follows the same principle as new-api's interrupted
// stream accounting: estimate from generated content seen before cancellation,
// not from the much larger SSE wire representation. A route with an unknown
// event shape still falls back to delivered bytes in estimate.
func (w *usageCapture) captureOutputDelta(data []byte) {
	var event struct {
		Type    string          `json:"type"`
		Delta   json.RawMessage `json:"delta"`
		Choices []struct {
			Text  string `json:"text"`
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
				ToolCalls        []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &event) != nil {
		return
	}
	add := func(value string) {
		if value != "" {
			w.outputBytes += int64(len(value))
			w.hasDelta = true
			if w.outputText.Len() < maxUsageCapture {
				remaining := maxUsageCapture - w.outputText.Len()
				if len(value) > remaining {
					value = value[:remaining]
				}
				w.outputText.WriteString(value)
			}
		}
	}
	if strings.HasSuffix(event.Type, ".delta") || event.Type == "content_block_delta" {
		var delta string
		if json.Unmarshal(event.Delta, &delta) == nil {
			add(delta)
		} else {
			var detail struct {
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			}
			if json.Unmarshal(event.Delta, &detail) == nil {
				add(detail.Text)
				add(detail.Thinking)
				add(detail.PartialJSON)
			}
		}
	}
	for _, choice := range event.Choices {
		add(choice.Text)
		add(choice.Delta.Content)
		add(choice.Delta.ReasoningContent)
		add(choice.Delta.Reasoning)
		for _, call := range choice.Delta.ToolCalls {
			add(call.Function.Arguments)
		}
	}
}

func (w *usageCapture) mergeUsage(usage tokenUsage) {
	w.hasUsage = true
	if usage.Input > w.usage.Input {
		w.usage.Input = usage.Input
	}
	if usage.Output > w.usage.Output {
		w.usage.Output = usage.Output
	}
	if usage.CacheRead > w.usage.CacheRead {
		w.usage.CacheRead = usage.CacheRead
	}
	if usage.CacheWrite > w.usage.CacheWrite {
		w.usage.CacheWrite = usage.CacheWrite
	}
}

func (w *usageCapture) result() (tokenUsage, error) {
	if w.status < 200 || w.status >= 300 {
		return tokenUsage{}, errors.New("request failed")
	}
	if w.route == "/v1/images/generations" {
		if !w.hasUsage || w.usage.Input+w.usage.Output == 0 {
			return tokenUsage{}, errors.New("upstream image response did not provide usable token usage")
		}
	} else if w.stream {
		w.finishEvent()
	} else if !w.tooLarge {
		if usage, ok := parseResponseUsage(w.route, w.body.Bytes()); ok {
			w.mergeUsage(usage)
		}
	}
	if w.tooLarge || !w.hasUsage || !w.usage.valid() {
		return tokenUsage{}, errors.New("upstream response did not provide usable token usage")
	}
	return w.usage, nil
}

// Unknown event formats and unparseable responses use a byte fallback.
const (
	estimatedResponseBytesPerToken = 4
)

// estimate bounds token usage from what was sent and delivered, keeping any
// exact counts already reported, such as Anthropic's message_start input.
func (w *usageCapture) estimate(requestBody []byte) tokenUsage {
	if w.stream {
		w.finishEvent()
	}
	if w.model == "" {
		var request struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(requestBody, &request)
		w.model = request.Model
	}
	usage := tokenUsage{}
	if w.hasUsage && w.usage.valid() && w.usage.Input > 0 {
		usage.Input, usage.CacheRead, usage.CacheWrite = w.usage.Input, w.usage.CacheRead, w.usage.CacheWrite
	} else {
		usage.Input = estimateRequestTokens(w.route, requestBody, w.model)
	}
	outputBytes := w.written
	if w.stream && w.hasDelta {
		usage.Output = int64(tokenkit.Estimate(w.model, w.outputText.String()))
		if remaining := w.outputBytes - int64(w.outputText.Len()); remaining > 0 {
			usage.Output += (remaining + estimatedResponseBytesPerToken - 1) / estimatedResponseBytesPerToken
		}
	} else {
		usage.Output = outputBytes / estimatedResponseBytesPerToken
	}
	if w.hasUsage && w.usage.Output > usage.Output {
		usage.Output = w.usage.Output
	}
	return usage
}

func parseResponseUsage(route string, data []byte) (tokenUsage, bool) {
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		return tokenUsage{}, false
	}
	raw := root["usage"]
	if len(raw) == 0 {
		for _, field := range []string{"response", "message"} {
			var nested map[string]json.RawMessage
			if json.Unmarshal(root[field], &nested) == nil {
				raw = nested["usage"]
				if len(raw) > 0 {
					break
				}
			}
		}
	}
	if len(raw) == 0 {
		return tokenUsage{}, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return tokenUsage{}, false
	}
	read := func(keys ...string) (int64, bool) {
		for _, key := range keys {
			if value, ok := fields[key]; ok {
				var count int64
				if json.Unmarshal(value, &count) != nil || count < 0 {
					return 0, false
				}
				return count, true
			}
		}
		return 0, false
	}
	input, inputOK := read("input_tokens", "prompt_tokens")
	output, outputOK := read("output_tokens", "completion_tokens")
	if route == "/v1/images/generations" && (!inputOK || !outputOK || output == 0) {
		return tokenUsage{}, false
	}
	if !inputOK && !outputOK {
		return tokenUsage{}, false
	}
	cacheRead, _ := read("cache_read_input_tokens")
	cacheWrite, _ := read("cache_creation_input_tokens")
	for _, detailsKey := range []string{"input_tokens_details", "prompt_tokens_details"} {
		var details map[string]json.RawMessage
		if json.Unmarshal(fields[detailsKey], &details) == nil {
			var cached int64
			if json.Unmarshal(details["cached_tokens"], &cached) == nil && cached >= 0 {
				cacheRead = cached
			}
		}
	}
	if route == "/v1/messages" {
		// Anthropic reports cache buckets separately from input_tokens.
		input += cacheRead + cacheWrite
	}
	return tokenUsage{Input: input, Output: output, CacheRead: cacheRead, CacheWrite: cacheWrite}, true
}

func requestUsageInStream(route string, body []byte) []byte {
	if route != "/v1/chat/completions" && route != "/v1/completions" {
		return body
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return body
	}
	var stream bool
	if json.Unmarshal(payload["stream"], &stream) != nil || !stream {
		return body
	}
	options := map[string]json.RawMessage{}
	if len(payload["stream_options"]) > 0 && json.Unmarshal(payload["stream_options"], &options) != nil {
		return body
	}
	options["include_usage"] = json.RawMessage("true")
	encoded, err := json.Marshal(options)
	if err != nil {
		return body
	}
	payload["stream_options"] = encoded
	encoded, err = json.Marshal(payload)
	if err != nil {
		return body
	}
	return encoded
}
