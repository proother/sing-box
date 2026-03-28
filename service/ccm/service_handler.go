package ccm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/buf"
	E "github.com/sagernet/sing/common/exceptions"

	"github.com/anthropics/anthropic-sdk-go"
)

const (
	contextWindowStandard   = 200000
	contextWindowPremium    = 1000000
	premiumContextThreshold = 200000
)

const (
	weeklyWindowSeconds = 604800
	weeklyWindowMinutes = weeklyWindowSeconds / 60
)

type ccmRequestMetadata struct {
	Model         string
	MessagesCount int
	SessionID     string
}

func isExtendedContextRequest(betaHeader string) bool {
	for _, feature := range strings.Split(betaHeader, ",") {
		if strings.HasPrefix(strings.TrimSpace(feature), "context-1m") {
			return true
		}
	}
	return false
}

func isFastModeRequest(betaHeader string) bool {
	for _, feature := range strings.Split(betaHeader, ",") {
		if strings.HasPrefix(strings.TrimSpace(feature), "fast-mode") {
			return true
		}
	}
	return false
}

func detectContextWindow(betaHeader string, totalInputTokens int64) int {
	if totalInputTokens > premiumContextThreshold {
		if isExtendedContextRequest(betaHeader) {
			return contextWindowPremium
		}
	}
	return contextWindowStandard
}

func extractWeeklyCycleHint(headers http.Header) *WeeklyCycleHint {
	resetAt, exists := parseOptionalAnthropicResetHeader(headers, "anthropic-ratelimit-unified-7d-reset")
	if !exists {
		return nil
	}

	return &WeeklyCycleHint{
		WindowMinutes: weeklyWindowMinutes,
		ResetAt:       resetAt.UTC(),
	}
}

// extractCCMSessionID extracts the session ID from the metadata.user_id field.
//
// Claude Code >= 2.1.78 (@anthropic-ai/claude-code) encodes user_id as:
//
//	JSON.stringify({device_id, account_uuid, session_id, ...extras})
//
// ref: cli.js L66() — metadata constructor
//
// Claude Code < 2.1.78 used a template literal:
//
//	`user_${deviceId}_account_${accountUuid}_session_${sessionId}`
//
// ref: cli.js qs() — old metadata constructor
//
// Returns ("", nil) when userID is empty.
// Returns error when user_id is present but in an unrecognized format.
func extractCCMSessionID(userID string) (string, error) {
	if userID == "" {
		return "", nil
	}

	// v2.1.78+ JSON object format
	var userIDObject struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal([]byte(userID), &userIDObject) == nil && userIDObject.SessionID != "" {
		return userIDObject.SessionID, nil
	}

	// legacy template literal format
	sessionIndex := strings.LastIndex(userID, "_session_")
	if sessionIndex >= 0 {
		return userID[sessionIndex+len("_session_"):], nil
	}

	return "", E.New("unrecognized metadata.user_id format: ", userID)
}

func extractCCMRequestMetadata(path string, bodyBytes []byte) (ccmRequestMetadata, error) {
	switch path {
	case "/v1/messages":
		var request anthropic.MessageNewParams
		if json.Unmarshal(bodyBytes, &request) != nil {
			return ccmRequestMetadata{}, nil
		}

		metadata := ccmRequestMetadata{
			Model:         string(request.Model),
			MessagesCount: len(request.Messages),
		}
		if request.Metadata.UserID.Valid() {
			sessionID, err := extractCCMSessionID(request.Metadata.UserID.Value)
			if err != nil {
				return ccmRequestMetadata{}, err
			}
			metadata.SessionID = sessionID
		}
		return metadata, nil
	case "/v1/messages/count_tokens":
		var request anthropic.MessageCountTokensParams
		if json.Unmarshal(bodyBytes, &request) != nil {
			return ccmRequestMetadata{}, nil
		}
		return ccmRequestMetadata{
			Model:         string(request.Model),
			MessagesCount: len(request.Messages),
		}, nil
	default:
		return ccmRequestMetadata{}, nil
	}
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := log.ContextWithNewID(r.Context())
	if r.URL.Path == "/ccm/v1/status" {
		s.handleStatusEndpoint(w, r)
		return
	}

	if r.URL.Path == "/ccm/v1/reverse" {
		s.handleReverseConnect(ctx, w, r)
		return
	}

	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		writeJSONError(w, r, http.StatusNotFound, "not_found_error", "Not found")
		return
	}

	if r.Header.Get("X-Api-Key") != "" || r.Header.Get("Api-Key") != "" {
		writeJSONError(w, r, http.StatusBadRequest, "invalid_request_error",
			"API key authentication is not supported; use Authorization: Bearer with a CCM user token")
		return
	}

	var username string
	if len(s.options.Users) > 0 {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			s.logger.WarnContext(ctx, "authentication failed for request from ", r.RemoteAddr, ": missing Authorization header")
			writeJSONError(w, r, http.StatusUnauthorized, "authentication_error", "missing api key")
			return
		}
		clientToken := strings.TrimPrefix(authHeader, "Bearer ")
		if clientToken == authHeader {
			s.logger.WarnContext(ctx, "authentication failed for request from ", r.RemoteAddr, ": invalid Authorization format")
			writeJSONError(w, r, http.StatusUnauthorized, "authentication_error", "invalid api key format")
			return
		}
		var ok bool
		username, ok = s.userManager.Authenticate(clientToken)
		if !ok {
			s.logger.WarnContext(ctx, "authentication failed for request from ", r.RemoteAddr, ": unknown key: ", clientToken)
			writeJSONError(w, r, http.StatusUnauthorized, "authentication_error", "invalid api key")
			return
		}
	}

	// Always read body to extract model and session ID
	var bodyBytes []byte
	var requestModel string
	var messagesCount int
	var sessionID string

	if r.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(r.Body)
		if err != nil {
			s.logger.ErrorContext(ctx, "read request body: ", err)
			writeJSONError(w, r, http.StatusInternalServerError, "api_error", "failed to read request body")
			return
		}

		requestMetadata, err := extractCCMRequestMetadata(r.URL.Path, bodyBytes)
		if err != nil {
			s.logger.ErrorContext(ctx, "invalid metadata format: ", err)
			writeJSONError(w, r, http.StatusInternalServerError, "api_error", "invalid metadata format")
			return
		}
		requestModel = requestMetadata.Model
		messagesCount = requestMetadata.MessagesCount
		sessionID = requestMetadata.SessionID
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}

	// Resolve credential provider and user config
	var provider credentialProvider
	var userConfig *option.CCMUser
	if len(s.options.Users) > 0 {
		userConfig = s.userConfigMap[username]
		var err error
		provider, err = credentialForUser(s.userConfigMap, s.providers, username)
		if err != nil {
			s.logger.ErrorContext(ctx, "resolve credential: ", err)
			writeJSONError(w, r, http.StatusInternalServerError, "api_error", err.Error())
			return
		}
	} else {
		provider = s.providers[s.options.Credentials[0].Tag]
	}
	if provider == nil {
		writeJSONError(w, r, http.StatusInternalServerError, "api_error", "no credential available")
		return
	}

	provider.pollIfStale()
	if userConfig != nil && userConfig.ExternalCredential != "" {
		for _, credential := range s.allCredentials {
			if credential.tagName() == userConfig.ExternalCredential && !credential.isUsable() {
				credential.pollUsage()
				break
			}
		}
	}
	s.cleanSessionModels()

	anthropicBetaHeader := r.Header.Get("anthropic-beta")
	if isFastModeRequest(anthropicBetaHeader) {
		if _, isSingle := provider.(*singleCredentialProvider); !isSingle {
			writeJSONError(w, r, http.StatusBadRequest, "invalid_request_error",
				"fast mode requests will consume Extra usage, please use a default credential directly")
			return
		}
	}

	selection := credentialSelectionForUser(userConfig)

	selectedCredential, isNew, err := provider.selectCredential(sessionID, selection)
	if err != nil {
		writeNonRetryableCredentialError(w, r, unavailableCredentialMessage(provider, err.Error()))
		return
	}
	modelDisplay := requestModel
	if requestModel != "" && isExtendedContextRequest(anthropicBetaHeader) {
		modelDisplay += "[1m]"
	}
	isNewModel := false
	if sessionID != "" && modelDisplay != "" {
		key := sessionModelKey{sessionID, modelDisplay}
		s.sessionModelAccess.Lock()
		_, exists := s.sessionModels[key]
		if !exists {
			s.sessionModels[key] = time.Now()
			isNewModel = true
		}
		s.sessionModelAccess.Unlock()
	}
	if isNew || isNewModel {
		logParts := []any{"assigned credential ", selectedCredential.tagName()}
		if sessionID != "" {
			logParts = append(logParts, " for session ", sessionID)
		}
		if username != "" {
			logParts = append(logParts, " by user ", username)
		}
		if modelDisplay != "" {
			logParts = append(logParts, ", model=", modelDisplay)
		}
		s.logger.DebugContext(ctx, logParts...)
	}

	if isFastModeRequest(anthropicBetaHeader) && selectedCredential.isExternal() {
		writeJSONError(w, r, http.StatusBadRequest, "invalid_request_error",
			"fast mode requests cannot be proxied through external credentials")
		return
	}

	requestContext := selectedCredential.wrapRequestContext(ctx)
	{
		currentRequestContext := requestContext
		requestContext.addInterruptLink(provider.linkProviderInterrupt(selectedCredential, selection, func() {
			currentRequestContext.cancelOnce.Do(currentRequestContext.cancelFunc)
		}))
	}
	defer func() {
		requestContext.cancelRequest()
	}()
	proxyRequest, err := selectedCredential.buildProxyRequest(requestContext, r, bodyBytes, s.httpHeaders)
	if err != nil {
		s.logger.ErrorContext(ctx, "create proxy request: ", err)
		writeJSONError(w, r, http.StatusInternalServerError, "api_error", "Internal server error")
		return
	}

	response, err := selectedCredential.httpClient().Do(proxyRequest)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		if requestContext.Err() != nil {
			writeCredentialUnavailableError(w, r, provider, selectedCredential, selection, "credential became unavailable while processing the request")
			return
		}
		writeJSONError(w, r, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	requestContext.releaseCredentialInterrupt()

	// Transparent 429 retry
	for response.StatusCode == http.StatusTooManyRequests {
		resetAt := parseRateLimitResetFromHeaders(response.Header)
		nextCredential := provider.onRateLimited(sessionID, selectedCredential, resetAt, selection)
		selectedCredential.updateStateFromHeaders(response.Header)
		if bodyBytes == nil || nextCredential == nil {
			response.Body.Close()
			writeCredentialUnavailableError(w, r, provider, selectedCredential, selection, "all credentials rate-limited")
			return
		}
		response.Body.Close()
		s.logger.InfoContext(ctx, "retrying with credential ", nextCredential.tagName(), " after 429 from ", selectedCredential.tagName())
		requestContext.cancelRequest()
		requestContext = nextCredential.wrapRequestContext(ctx)
		{
			currentRequestContext := requestContext
			requestContext.addInterruptLink(provider.linkProviderInterrupt(nextCredential, selection, func() {
				currentRequestContext.cancelOnce.Do(currentRequestContext.cancelFunc)
			}))
		}
		retryRequest, buildErr := nextCredential.buildProxyRequest(requestContext, r, bodyBytes, s.httpHeaders)
		if buildErr != nil {
			s.logger.ErrorContext(ctx, "retry request: ", buildErr)
			writeJSONError(w, r, http.StatusBadGateway, "api_error", buildErr.Error())
			return
		}
		retryResponse, retryErr := nextCredential.httpClient().Do(retryRequest)
		if retryErr != nil {
			if r.Context().Err() != nil {
				return
			}
			if requestContext.Err() != nil {
				writeCredentialUnavailableError(w, r, provider, nextCredential, selection, "credential became unavailable while retrying the request")
				return
			}
			s.logger.ErrorContext(ctx, "retry request: ", retryErr)
			writeJSONError(w, r, http.StatusBadGateway, "api_error", retryErr.Error())
			return
		}
		requestContext.releaseCredentialInterrupt()
		response = retryResponse
		selectedCredential = nextCredential
	}
	defer response.Body.Close()

	selectedCredential.updateStateFromHeaders(response.Header)

	if response.StatusCode == 529 {
		s.logger.WarnContext(ctx, "upstream overloaded from ", selectedCredential.tagName())
		for key, values := range response.Header {
			if !isHopByHopHeader(key) && !isReverseProxyHeader(key) {
				w.Header()[key] = values
			}
		}
		w.WriteHeader(response.StatusCode)
		io.Copy(w, response.Body)
		return
	}

	if response.StatusCode == http.StatusBadRequest {
		if selectedCredential.isExternal() {
			selectedCredential.markUpstreamRejected()
		} else {
			provider.pollCredentialIfStale(selectedCredential)
		}
		s.logger.ErrorContext(ctx, "upstream rejected from ", selectedCredential.tagName(), ": status ", response.StatusCode)
		writeCredentialUnavailableError(w, r, provider, selectedCredential, selection, "upstream rejected credential")
		return
	}

	// ref (@anthropic-ai/claude-code @2.1.81): cli.js NA9 (line 179488-179494) — 401 recovery
	// ref: cli.js CR1 (line 314268-314273) — 403 "OAuth token has been revoked" recovery
	if !selectedCredential.isExternal() && bodyBytes != nil &&
		(response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) {
		shouldRetry := response.StatusCode == http.StatusUnauthorized
		var peekBody []byte
		if response.StatusCode == http.StatusForbidden {
			peekBody, _ = io.ReadAll(response.Body)
			shouldRetry = strings.Contains(string(peekBody), "OAuth token has been revoked")
			if !shouldRetry {
				response.Body.Close()
				s.logger.ErrorContext(ctx, "upstream error from ", selectedCredential.tagName(), ": status ", response.StatusCode, " ", string(peekBody))
				writeJSONError(w, r, http.StatusInternalServerError, "api_error",
					"proxy request (status "+strconv.Itoa(response.StatusCode)+"): "+string(peekBody))
				return
			}
		}
		if shouldRetry {
			recovered := false
			if defaultCred, ok := selectedCredential.(*defaultCredential); ok {
				failedAccessToken := ""
				currentCredentials := defaultCred.currentCredentials()
				if currentCredentials != nil {
					failedAccessToken = currentCredentials.AccessToken
				}
				s.logger.WarnContext(ctx, "upstream auth failure from ", selectedCredential.tagName(), ", reloading credentials and retrying")
				recovered = defaultCred.recoverAuthFailure(failedAccessToken)
			}
			if recovered {
				response.Body.Close()
				retryRequest, buildErr := selectedCredential.buildProxyRequest(requestContext, r, bodyBytes, s.httpHeaders)
				if buildErr != nil {
					writeJSONError(w, r, http.StatusBadGateway, "api_error", E.Cause(buildErr, "rebuild request after auth recovery").Error())
					return
				}
				retryResponse, retryErr := selectedCredential.httpClient().Do(retryRequest)
				if retryErr != nil {
					writeJSONError(w, r, http.StatusBadGateway, "api_error", E.Cause(retryErr, "retry request after auth recovery").Error())
					return
				}
				response = retryResponse
				defer retryResponse.Body.Close()
			} else if response.StatusCode == http.StatusForbidden {
				response.Body = io.NopCloser(bytes.NewReader(peekBody))
			}
		}
	}

	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusTooManyRequests {
		body, _ := io.ReadAll(response.Body)
		s.logger.ErrorContext(ctx, "upstream error from ", selectedCredential.tagName(), ": status ", response.StatusCode, " ", string(body))
		writeJSONError(w, r, http.StatusInternalServerError, "api_error",
			"proxy request (status "+strconv.Itoa(response.StatusCode)+"): "+string(body))
		return
	}

	s.rewriteResponseHeaders(response.Header, provider, userConfig)

	for key, values := range response.Header {
		if !isHopByHopHeader(key) && !isReverseProxyHeader(key) {
			w.Header()[key] = values
		}
	}
	w.WriteHeader(response.StatusCode)

	usageTracker := selectedCredential.usageTrackerOrNil()
	if usageTracker != nil && response.StatusCode == http.StatusOK {
		s.handleResponseWithTracking(ctx, w, response, usageTracker, requestModel, anthropicBetaHeader, messagesCount, username)
	} else {
		mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
		if err == nil && mediaType != "text/event-stream" {
			_, _ = io.Copy(w, response.Body)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			s.logger.ErrorContext(ctx, "streaming not supported")
			return
		}
		buffer := make([]byte, buf.BufferSize)
		for {
			n, err := response.Body.Read(buffer)
			if n > 0 {
				_, writeError := w.Write(buffer[:n])
				if writeError != nil {
					if E.IsClosedOrCanceled(writeError) {
						return
					}
					s.logger.ErrorContext(ctx, "write streaming response: ", writeError)
					return
				}
				flusher.Flush()
			}
			if err != nil {
				return
			}
		}
	}
}

func (s *Service) handleResponseWithTracking(ctx context.Context, writer http.ResponseWriter, response *http.Response, usageTracker *AggregatedUsage, requestModel string, anthropicBetaHeader string, messagesCount int, username string) {
	weeklyCycleHint := extractWeeklyCycleHint(response.Header)
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	isStreaming := err == nil && mediaType == "text/event-stream"

	if !isStreaming {
		bodyBytes, err := io.ReadAll(response.Body)
		if err != nil {
			s.logger.ErrorContext(ctx, "read response body: ", err)
			return
		}

		var message anthropic.Message
		var usage anthropic.Usage
		var responseModel string
		err = json.Unmarshal(bodyBytes, &message)
		if err == nil {
			responseModel = string(message.Model)
			usage = message.Usage
		}
		if responseModel == "" {
			responseModel = requestModel
		}

		if usage.InputTokens > 0 || usage.OutputTokens > 0 {
			if responseModel != "" {
				totalInputTokens := usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens
				contextWindow := detectContextWindow(anthropicBetaHeader, totalInputTokens)
				usageTracker.AddUsageWithCycleHint(
					responseModel,
					contextWindow,
					messagesCount,
					usage.InputTokens,
					usage.OutputTokens,
					usage.CacheReadInputTokens,
					usage.CacheCreationInputTokens,
					usage.CacheCreation.Ephemeral5mInputTokens,
					usage.CacheCreation.Ephemeral1hInputTokens,
					username,
					time.Now(),
					weeklyCycleHint,
				)
			}
		}

		_, _ = writer.Write(bodyBytes)
		return
	}

	flusher, ok := writer.(http.Flusher)
	if !ok {
		s.logger.ErrorContext(ctx, "streaming not supported")
		return
	}

	var accumulatedUsage anthropic.Usage
	var responseModel string
	buffer := make([]byte, buf.BufferSize)
	var leftover []byte

	for {
		n, err := response.Body.Read(buffer)
		if n > 0 {
			data := append(leftover, buffer[:n]...)
			lines := bytes.Split(data, []byte("\n"))

			if err == nil {
				leftover = lines[len(lines)-1]
				lines = lines[:len(lines)-1]
			} else {
				leftover = nil
			}

			for _, line := range lines {
				line = bytes.TrimSpace(line)
				if len(line) == 0 {
					continue
				}

				if bytes.HasPrefix(line, []byte("data: ")) {
					eventData := bytes.TrimPrefix(line, []byte("data: "))
					if bytes.Equal(eventData, []byte("[DONE]")) {
						continue
					}

					var event anthropic.MessageStreamEventUnion
					err := json.Unmarshal(eventData, &event)
					if err != nil {
						continue
					}
					switch event.Type {
					case "message_start":
						messageStart := event.AsMessageStart()
						if messageStart.Message.Model != "" {
							responseModel = string(messageStart.Message.Model)
						}
						if messageStart.Message.Usage.InputTokens > 0 {
							accumulatedUsage.InputTokens = messageStart.Message.Usage.InputTokens
							accumulatedUsage.CacheReadInputTokens = messageStart.Message.Usage.CacheReadInputTokens
							accumulatedUsage.CacheCreationInputTokens = messageStart.Message.Usage.CacheCreationInputTokens
							accumulatedUsage.CacheCreation.Ephemeral5mInputTokens = messageStart.Message.Usage.CacheCreation.Ephemeral5mInputTokens
							accumulatedUsage.CacheCreation.Ephemeral1hInputTokens = messageStart.Message.Usage.CacheCreation.Ephemeral1hInputTokens
						}
					case "message_delta":
						messageDelta := event.AsMessageDelta()
						if messageDelta.Usage.OutputTokens > 0 {
							accumulatedUsage.OutputTokens = messageDelta.Usage.OutputTokens
						}
					}
				}
			}

			_, writeError := writer.Write(buffer[:n])
			if writeError != nil {
				if E.IsClosedOrCanceled(writeError) {
					return
				}
				s.logger.ErrorContext(ctx, "write streaming response: ", writeError)
				return
			}
			flusher.Flush()
		}

		if err != nil {
			if responseModel == "" {
				responseModel = requestModel
			}

			if accumulatedUsage.InputTokens > 0 || accumulatedUsage.OutputTokens > 0 {
				if responseModel != "" {
					totalInputTokens := accumulatedUsage.InputTokens + accumulatedUsage.CacheCreationInputTokens + accumulatedUsage.CacheReadInputTokens
					contextWindow := detectContextWindow(anthropicBetaHeader, totalInputTokens)
					usageTracker.AddUsageWithCycleHint(
						responseModel,
						contextWindow,
						messagesCount,
						accumulatedUsage.InputTokens,
						accumulatedUsage.OutputTokens,
						accumulatedUsage.CacheReadInputTokens,
						accumulatedUsage.CacheCreationInputTokens,
						accumulatedUsage.CacheCreation.Ephemeral5mInputTokens,
						accumulatedUsage.CacheCreation.Ephemeral1hInputTokens,
						username,
						time.Now(),
						weeklyCycleHint,
					)
				}
			}
			return
		}
	}
}
