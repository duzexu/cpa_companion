package companion

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCyberPolicyDetectorRecognizesStructuredResponses(t *testing.T) {
	for _, raw := range []string{
		`{"error":{"type":"invalid_request","code":"cyber_policy","message":"flagged"}}`,
		`{"type":"response.failed","response":{"error":{"code":"cyber_policy"}}}`,
		`{"type":"error","code":"cyber_policy"}`,
		`{"error":{"code":"cyber_abuse"}}`,
	} {
		if !isCyberPolicyError([]byte(raw)) {
			t.Fatalf("missed structured cyber policy error: %s", raw)
		}
	}
	for _, raw := range []string{
		`{"output":"The phrase cyber_policy appears in a document."}`,
		`{"error":{"code":"rate_limit_exceeded","message":"cyber_policy"}}`,
		`{"message":"cyber abuse"}`,
	} {
		if isCyberPolicyError([]byte(raw)) {
			t.Fatalf("false positive cyber policy error: %s", raw)
		}
	}
}

func TestCyberPolicyDetectionPreservesForwardedJSONAndSSE(t *testing.T) {
	jsonBody := `{"error":{"type":"invalid_request","code":"cyber_policy","message":"flagged"}}`
	jsonTarget := httptest.NewRecorder()
	jsonWriter := &retryResponseWriter{target: jsonTarget, header: make(http.Header), watchCyber: true}
	jsonWriter.Header().Set("Content-Type", "application/json")
	jsonWriter.WriteHeader(http.StatusBadRequest)
	_, _ = jsonWriter.Write([]byte(jsonBody))
	jsonWriter.commit()
	if !jsonWriter.cyberPolicyDetected() || jsonTarget.Code != http.StatusBadRequest || jsonTarget.Body.String() != jsonBody {
		t.Fatalf("JSON response changed or marker missed: %d %q", jsonTarget.Code, jsonTarget.Body.String())
	}

	sseBody := "event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":\"cyber_policy\"}}\n\n"
	sseTarget := httptest.NewRecorder()
	sseWriter := &retryResponseWriter{target: sseTarget, header: make(http.Header), watchCyber: true}
	sseWriter.Header().Set("Content-Type", "text/event-stream")
	sseWriter.WriteHeader(http.StatusOK)
	for _, chunk := range []string{sseBody[:40], sseBody[40:84], sseBody[84:]} {
		_, _ = sseWriter.Write([]byte(chunk))
	}
	sseWriter.commit()
	if !sseWriter.cyberPolicyDetected() || sseTarget.Code != http.StatusOK || sseTarget.Body.String() != sseBody {
		t.Fatalf("SSE response changed or marker missed: %d %q", sseTarget.Code, sseTarget.Body.String())
	}

	normalTarget := httptest.NewRecorder()
	normal := &retryResponseWriter{target: normalTarget, header: make(http.Header), watchCyber: true}
	normal.Header().Set("Content-Type", "text/event-stream")
	normal.WriteHeader(http.StatusOK)
	_, _ = normal.Write([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"cyber_policy\"}\n\n"))
	if normal.cyberPolicyDetected() {
		t.Fatal("ordinary model text triggered cyber policy audit")
	}
}

func TestUpstreamCyberPolicyWritesAuditWithoutChangingRequest(t *testing.T) {
	logs, err := openUsageLogs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	a := &app{logs: logs}
	request := []byte(`{"model":"test","messages":[{"role":"user","content":"older"},{"role":"assistant","content":"reply"},{"role":"user","content":"current input"}]}`)
	usage := &usageLog{UserID: "user", Username: "AliceName", KeyID: "key", PlanID: "plan", Model: "test", Route: "/v1/responses"}
	if err := a.recordUpstreamCyberPolicy(request, usage, "cyber_policy"); err != nil {
		t.Fatal(err)
	}
	rows, _, err := logs.auditRecords(context.Background(), "user", "", 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("expected one upstream audit record: %d, %v", len(rows), err)
	}
	if rows[0].Username != "AliceName" || rows[0].InputText != "用户：\nolder\n\n助手：\nreply\n\n用户：\ncurrent input" || rows[0].Category != "cyber_policy" || rows[0].SourceText() != "OpenAI 上游" || rows[0].ReasonText() != "上游标记" {
		t.Fatalf("unexpected upstream audit record: %+v", rows[0])
	}
}
