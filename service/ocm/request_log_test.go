package ocm

import (
	"strings"
	"testing"

	F "github.com/sagernet/sing/common/format"
)

func TestParseRequestLogMetadata(t *testing.T) {
	t.Parallel()

	metadata := parseRequestLogMetadata("/v1/responses", []byte(`{
		"model":"gpt-5.4",
		"service_tier":"priority",
		"reasoning":{"effort":"xhigh"}
	}`))

	if metadata.Model != "gpt-5.4" {
		t.Fatalf("expected model gpt-5.4, got %q", metadata.Model)
	}
	if metadata.ServiceTier != "priority" {
		t.Fatalf("expected priority service tier, got %q", metadata.ServiceTier)
	}
	if metadata.ReasoningEffort != "xhigh" {
		t.Fatalf("expected xhigh reasoning effort, got %q", metadata.ReasoningEffort)
	}
}

func TestParseRequestLogMetadataFallsBackToTopLevelReasoningEffort(t *testing.T) {
	t.Parallel()

	metadata := parseRequestLogMetadata("/v1/responses", []byte(`{
		"model":"gpt-5.4",
		"reasoning_effort":"high"
	}`))

	if metadata.ReasoningEffort != "high" {
		t.Fatalf("expected high reasoning effort, got %q", metadata.ReasoningEffort)
	}
}

func TestParseRequestLogMetadataFromChatCompletions(t *testing.T) {
	t.Parallel()

	metadata := parseRequestLogMetadata("/v1/chat/completions", []byte(`{
		"model":"gpt-5.4",
		"service_tier":"priority",
		"reasoning_effort":"xhigh",
		"messages":[{"role":"user","content":"hi"}]
	}`))

	if metadata.Model != "gpt-5.4" {
		t.Fatalf("expected model gpt-5.4, got %q", metadata.Model)
	}
	if metadata.ServiceTier != "priority" {
		t.Fatalf("expected priority service tier, got %q", metadata.ServiceTier)
	}
	if metadata.ReasoningEffort != "xhigh" {
		t.Fatalf("expected xhigh reasoning effort, got %q", metadata.ReasoningEffort)
	}
}

func TestParseRequestLogMetadataIgnoresUnsupportedPath(t *testing.T) {
	t.Parallel()

	metadata := parseRequestLogMetadata("/v1/files", []byte(`{"model":"gpt-5.4"}`))
	if metadata != (requestLogMetadata{}) {
		t.Fatalf("expected zero metadata, got %#v", metadata)
	}
}

func TestBuildAssignedCredentialLogPartsIncludesThinkLevel(t *testing.T) {
	t.Parallel()

	message := F.ToString(buildAssignedCredentialLogParts("a", "session-1", "alice", requestLogMetadata{
		Model:           "gpt-5.4",
		ServiceTier:     "priority",
		ReasoningEffort: "xhigh",
	})...)

	for _, fragment := range []string{
		"assigned credential a",
		"for session session-1",
		"by user alice",
		"model=gpt-5.4",
		"think=xhigh",
		"fast",
	} {
		if !strings.Contains(message, fragment) {
			t.Fatalf("expected %q in %q", fragment, message)
		}
	}
}

func TestParseWebSocketResponseCreateRequestIncludesThinkLevel(t *testing.T) {
	t.Parallel()

	request, ok := parseWebSocketResponseCreateRequest([]byte(`{
		"type":"response.create",
		"model":"gpt-5.4",
		"reasoning":{"effort":"xhigh"}
	}`))
	if !ok {
		t.Fatal("expected websocket response.create request to parse")
	}
	if request.metadata().ReasoningEffort != "xhigh" {
		t.Fatalf("expected xhigh reasoning effort, got %q", request.metadata().ReasoningEffort)
	}
}

func TestParseWebSocketResponseCreateRequestFallsBackToLegacyReasoningEffort(t *testing.T) {
	t.Parallel()

	request, ok := parseWebSocketResponseCreateRequest([]byte(`{
		"type":"response.create",
		"model":"gpt-5.4",
		"reasoning_effort":"high"
	}`))
	if !ok {
		t.Fatal("expected websocket response.create request to parse")
	}
	if request.metadata().ReasoningEffort != "high" {
		t.Fatalf("expected high reasoning effort, got %q", request.metadata().ReasoningEffort)
	}
}
