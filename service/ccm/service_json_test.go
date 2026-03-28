package ccm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestWriteJSONErrorUsesAnthropicShape(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
	request.Header.Set("Request-Id", "req_123")

	writeJSONError(recorder, request, http.StatusBadRequest, "invalid_request_error", "broken")

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}

	var body anthropic.ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	if string(body.Type) != "error" {
		t.Fatalf("expected error type, got %q", body.Type)
	}
	if body.RequestID != "req_123" {
		t.Fatalf("expected req_123 request ID, got %q", body.RequestID)
	}
	if body.Error.Type != "invalid_request_error" {
		t.Fatalf("expected invalid_request_error, got %q", body.Error.Type)
	}
	if body.Error.Message != "broken" {
		t.Fatalf("expected broken message, got %q", body.Error.Message)
	}
}

func TestExtractCCMRequestMetadataFromMessagesJSONSession(t *testing.T) {
	t.Parallel()

	metadata, err := extractCCMRequestMetadata("/v1/messages", []byte(`{
		"model":"claude-sonnet-4-5",
		"max_tokens":1,
		"messages":[{"role":"user","content":"hello"}],
		"metadata":{"user_id":"{\"session_id\":\"session-1\"}"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Model != "claude-sonnet-4-5" {
		t.Fatalf("expected model, got %#v", metadata)
	}
	if metadata.MessagesCount != 1 {
		t.Fatalf("expected one message, got %#v", metadata)
	}
	if metadata.SessionID != "session-1" {
		t.Fatalf("expected session-1, got %#v", metadata)
	}
}

func TestExtractCCMRequestMetadataFromMessagesLegacySession(t *testing.T) {
	t.Parallel()

	metadata, err := extractCCMRequestMetadata("/v1/messages", []byte(`{
		"model":"claude-sonnet-4-5",
		"max_tokens":1,
		"messages":[{"role":"user","content":"hello"}],
		"metadata":{"user_id":"user_device_account_account_session_session-legacy"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.SessionID != "session-legacy" {
		t.Fatalf("expected session-legacy, got %#v", metadata)
	}
}

func TestExtractCCMRequestMetadataFromCountTokens(t *testing.T) {
	t.Parallel()

	metadata, err := extractCCMRequestMetadata("/v1/messages/count_tokens", []byte(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"hello"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Model != "claude-sonnet-4-5" {
		t.Fatalf("expected model, got %#v", metadata)
	}
	if metadata.MessagesCount != 1 {
		t.Fatalf("expected one message, got %#v", metadata)
	}
	if metadata.SessionID != "" {
		t.Fatalf("expected empty session ID, got %#v", metadata)
	}
}

func TestExtractCCMRequestMetadataIgnoresUnsupportedPath(t *testing.T) {
	t.Parallel()

	metadata, err := extractCCMRequestMetadata("/v1/models", []byte(`{"model":"claude"}`))
	if err != nil {
		t.Fatal(err)
	}
	if metadata != (ccmRequestMetadata{}) {
		t.Fatalf("expected zero metadata, got %#v", metadata)
	}
}
