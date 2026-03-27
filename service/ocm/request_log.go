package ocm

import "encoding/json"

type requestLogMetadata struct {
	Model           string
	ServiceTier     string
	ReasoningEffort string
}

type requestLogReasoning struct {
	Effort string `json:"effort"`
}

type requestLogPayload struct {
	Model           string               `json:"model"`
	ServiceTier     string               `json:"service_tier"`
	Reasoning       *requestLogReasoning `json:"reasoning"`
	ReasoningEffort string               `json:"reasoning_effort"`
}

func (p requestLogPayload) metadata() requestLogMetadata {
	metadata := requestLogMetadata{
		Model:       p.Model,
		ServiceTier: p.ServiceTier,
	}
	if p.Reasoning != nil {
		metadata.ReasoningEffort = p.Reasoning.Effort
	}
	if metadata.ReasoningEffort == "" {
		metadata.ReasoningEffort = p.ReasoningEffort
	}
	return metadata
}

func parseRequestLogMetadata(data []byte) requestLogMetadata {
	var payload requestLogPayload
	if json.Unmarshal(data, &payload) != nil {
		return requestLogMetadata{}
	}
	return payload.metadata()
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
