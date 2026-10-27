package companion

import "testing"

func TestEstimateRequestTokensUsesRequestContentAndNotJSONFraming(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"你好，世界"}]}`)
	got := estimateRequestTokens("/v1/chat/completions", body, "gpt-5.6-sol")
	if got <= 0 || got >= int64(len(body)) {
		t.Fatalf("request token estimate = %d, want positive count below JSON byte length", got)
	}
	withImage := []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}]}`)
	if got := estimateRequestTokens("/v1/chat/completions", withImage, "gpt-5.6-sol"); got <= estimateRequestTokens("/v1/chat/completions", body, "gpt-5.6-sol") {
		t.Fatalf("image content did not add media tokens: %d", got)
	}
}

func TestEstimateRequestTokensCountsResponsesHistoryAndTools(t *testing.T) {
	base := []byte(`{"model":"gpt-5.6-sol","input":"hello"}`)
	full := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},{"type":"function_call_output","output":"result"}],"tools":[{"type":"function","name":"lookup","description":"find data","parameters":{"type":"object"}}]}`)
	if got, want := estimateRequestTokens("/v1/responses", full, "gpt-5.6-sol"), estimateRequestTokens("/v1/responses", base, "gpt-5.6-sol"); got <= want {
		t.Fatalf("responses history/tools estimate=%d, base=%d", got, want)
	}
}
