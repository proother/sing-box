package ocm

import (
	"encoding/json"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

type requestLogMetadata struct {
	Model           string
	ServiceTier     string
	ReasoningEffort string
}

type legacyReasoningEffortPayload struct {
	ReasoningEffort string `json:"reasoning_effort"`
}

func requestLogMetadataFromChatCompletionRequest(request openai.ChatCompletionNewParams) requestLogMetadata {
	return requestLogMetadata{
		Model:           string(request.Model),
		ServiceTier:     string(request.ServiceTier),
		ReasoningEffort: string(request.ReasoningEffort),
	}
}

func requestLogMetadataFromResponsesRequest(request responses.ResponseNewParams, legacyReasoningEffort string) requestLogMetadata {
	metadata := requestLogMetadata{
		Model:       string(request.Model),
		ServiceTier: string(request.ServiceTier),
	}
	if request.Reasoning.Effort != "" {
		metadata.ReasoningEffort = string(request.Reasoning.Effort)
	}
	if metadata.ReasoningEffort == "" {
		metadata.ReasoningEffort = legacyReasoningEffort
	}
	return metadata
}

func parseLegacyReasoningEffort(data []byte) string {
	var legacy legacyReasoningEffortPayload
	if json.Unmarshal(data, &legacy) != nil {
		return ""
	}
	return legacy.ReasoningEffort
}

func parseRequestLogMetadata(path string, data []byte) requestLogMetadata {
	switch {
	case path == "/v1/chat/completions":
		var request openai.ChatCompletionNewParams
		if json.Unmarshal(data, &request) != nil {
			return requestLogMetadata{}
		}
		return requestLogMetadataFromChatCompletionRequest(request)
	case strings.HasPrefix(path, "/v1/responses"):
		var request responses.ResponseNewParams
		if json.Unmarshal(data, &request) != nil {
			return requestLogMetadata{}
		}
		return requestLogMetadataFromResponsesRequest(request, parseLegacyReasoningEffort(data))
	default:
		return requestLogMetadata{}
	}
}

func buildAssignedCredentialLogParts(credentialTag string, sessionID string, username string, metadata requestLogMetadata) []any {
	logParts := []any{"assigned credential ", credentialTag}
	if sessionID != "" {
		logParts = append(logParts, " for session ", sessionID)
	}
	if username != "" {
		logParts = append(logParts, " by user ", username)
	}
	if metadata.Model != "" {
		logParts = append(logParts, ", model=", metadata.Model)
	}
	if metadata.ReasoningEffort != "" {
		logParts = append(logParts, ", think=", metadata.ReasoningEffort)
	}
	if metadata.ServiceTier == "priority" {
		logParts = append(logParts, ", fast")
	}
	return logParts
}
