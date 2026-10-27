package companion

import (
	"encoding/json"
	"strings"

	"github.com/QuantumNous/new-api/tokenkit"
)

// estimateRequestTokens mirrors new-api's request-token path for the public
// JSON routes: count the prompt content with tokenkit, then add OpenAI chat
// message, name and tool framing. Media is charged by type, not by the length
// of a data URL or base64 payload.
func estimateRequestTokens(route string, body []byte, model string) int64 {
	var request map[string]any
	if json.Unmarshal(body, &request) != nil {
		return int64(tokenkit.Count(model, string(body)))
	}
	var text []string
	var media int64
	add := func(value string) {
		if value != "" {
			text = append(text, value)
		}
	}
	addJSON := func(value any) {
		if value != nil {
			encoded, _ := json.Marshal(value)
			add(string(encoded))
		}
	}
	addValue := func(value any) {
		switch v := value.(type) {
		case string:
			add(v)
		case []any:
			for _, part := range v {
				if s, ok := part.(string); ok {
					add(s)
				}
			}
		}
	}
	var walkContent func(any)
	walkContent = func(value any) {
		switch v := value.(type) {
		case string:
			add(v)
		case []any:
			for _, item := range v {
				walkContent(item)
			}
		case map[string]any:
			typ, _ := v["type"].(string)
			switch typ {
			case "image", "image_url", "input_image":
				detail, _ := v["detail"].(string)
				if image, ok := v["image_url"].(map[string]any); ok {
					if d, ok := image["detail"].(string); ok {
						detail = d
					}
				}
				media += int64(tokenkit.ImageTokens(model, tokenkit.Image{Detail: detail}))
			case "input_audio", "audio":
				media += 256
			case "video_url", "video":
				media += 8192
			case "input_file", "file":
				media += 4096
			case "tool_use":
				add(textField(v, "name"))
				addJSON(v["input"])
			case "tool_result":
				walkContent(v["content"])
			default:
				add(textField(v, "text"))
			}
		}
	}
	var overhead int64
	switch route {
	case "/v1/chat/completions", "/v1/completions":
		addValue(request["prompt"])
		addValue(request["input"])
		if messages, ok := request["messages"].([]any); ok {
			for _, item := range messages {
				message, ok := item.(map[string]any)
				if !ok {
					continue
				}
				overhead += 3
				add(textField(message, "role"))
				if name := textField(message, "name"); name != "" {
					overhead += 3
					add(name)
				}
				walkContent(message["content"])
				if calls, ok := message["tool_calls"].([]any); ok {
					for _, item := range calls {
						call, ok := item.(map[string]any)
						if !ok {
							continue
						}
						if function, ok := call["function"].(map[string]any); ok {
							add(textField(function, "name"))
							add(textField(function, "arguments"))
						} else {
							addJSON(call["custom"])
						}
					}
				}
			}
		}
		if tools, ok := request["tools"].([]any); ok {
			for _, item := range tools {
				tool, ok := item.(map[string]any)
				if !ok {
					continue
				}
				overhead += 8
				if function, ok := tool["function"].(map[string]any); ok {
					add(textField(function, "name"))
					add(textField(function, "description"))
					addJSON(function["parameters"])
				} else {
					addJSON(tool["custom"])
				}
			}
		}
		overhead += 3
	case "/v1/responses":
		if input, ok := request["input"].([]any); ok {
			for _, item := range input {
				entry, ok := item.(map[string]any)
				if !ok {
					continue
				}
				add(textField(entry, "name"))
				content := entry["content"]
				switch textField(entry, "type") {
				case "function_call":
					content = entry["arguments"]
				case "custom_tool_call":
					content = entry["tool_input"]
				case "function_call_output", "custom_tool_call_output", "local_shell_call_output":
					content = entry["output"]
				}
				walkContent(content)
			}
		} else {
			addValue(request["input"])
		}
		for _, field := range []string{"instructions", "metadata", "text", "tool_choice", "prompt", "tools"} {
			if value, ok := request[field].(string); ok {
				add(value)
			} else {
				addJSON(request[field])
			}
		}
		overhead += 3
	case "/v1/responses/compact":
		addJSON(request["instructions"])
		addJSON(request["input"])
		overhead += 3
	case "/v1/messages":
		walkContent(request["system"])
		if messages, ok := request["messages"].([]any); ok {
			for _, item := range messages {
				if message, ok := item.(map[string]any); ok {
					add(textField(message, "role"))
					walkContent(message["content"])
				}
			}
		}
		if tools, ok := request["tools"].([]any); ok {
			for _, item := range tools {
				if tool, ok := item.(map[string]any); ok {
					add(textField(tool, "name"))
					add(textField(tool, "description"))
					addJSON(tool["input_schema"])
				}
			}
		}
	case "/v1/images/generations":
		add(textField(request, "prompt"))
	case "/v1/alpha/search":
		add(string(body))
	default:
		add(string(body))
	}
	return int64(tokenkit.Count(model, strings.Join(text, "\n"))) + media + overhead
}

func textField(item map[string]any, name string) string {
	value, _ := item[name].(string)
	return value
}
