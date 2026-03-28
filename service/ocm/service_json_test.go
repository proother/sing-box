package ocm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestWriteJSONErrorIncludesSDKErrorFields(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	writeJSONErrorWithCode(recorder, request, http.StatusBadRequest, "invalid_request_error", "bad_thing", "broken")

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}

	var body struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"type", "message", "code", "param"} {
		if _, exists := body.Error[key]; !exists {
			t.Fatalf("expected error.%s to be present, got %#v", key, body.Error)
		}
	}
	if body.Error["type"] != "invalid_request_error" {
		t.Fatalf("expected invalid_request_error type, got %#v", body.Error["type"])
	}
	if body.Error["message"] != "broken" {
		t.Fatalf("expected broken message, got %#v", body.Error["message"])
	}
	if body.Error["code"] != "bad_thing" {
		t.Fatalf("expected bad_thing code, got %#v", body.Error["code"])
	}
	if body.Error["param"] != "" {
		t.Fatalf("expected empty param, got %#v", body.Error["param"])
	}
}

func TestHandleWebSocketErrorEventRateLimitTracksHeadersAndReset(t *testing.T) {
	t.Parallel()

	credential := &testCredential{availability: availabilityStatus{State: availabilityStateUsable}}
	service := &Service{}
	resetAt := time.Now().Add(time.Minute).Unix()

	service.handleWebSocketErrorEvent([]byte(`{
		"type":"error",
		"status_code":429,
		"headers":{
			"x-codex-active-limit":"codex",
			"x-codex-primary-reset-at":"`+strconv.FormatInt(resetAt, 10)+`"
		},
		"error":{
			"type":"rate_limit_error",
			"code":"rate_limited",
			"message":"limit hit",
			"param":""
		}
	}`), credential)

	if credential.lastHeaders.Get("x-codex-active-limit") != "codex" {
		t.Fatalf("expected headers to be forwarded, got %#v", credential.lastHeaders)
	}
	if credential.rateLimitedAt.Unix() != resetAt {
		t.Fatalf("expected reset %d, got %d", resetAt, credential.rateLimitedAt.Unix())
	}
}
