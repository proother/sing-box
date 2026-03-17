package ocm

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sagernet/sing-box/option"
)

type statusPayload struct {
	FiveHourUtilization float64 `json:"five_hour_utilization"`
	FiveHourReset       int64   `json:"five_hour_reset"`
	WeeklyUtilization   float64 `json:"weekly_utilization"`
	WeeklyReset         int64   `json:"weekly_reset"`
	PlanWeight          float64 `json:"plan_weight"`
}

type aggregatedStatus struct {
	fiveHourUtilization float64
	weeklyUtilization   float64
	totalWeight         float64
	fiveHourReset       time.Time
	weeklyReset         time.Time
}

func resetToEpoch(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func (s aggregatedStatus) equal(other aggregatedStatus) bool {
	return s.fiveHourUtilization == other.fiveHourUtilization &&
		s.weeklyUtilization == other.weeklyUtilization &&
		s.totalWeight == other.totalWeight &&
		resetToEpoch(s.fiveHourReset) == resetToEpoch(other.fiveHourReset) &&
		resetToEpoch(s.weeklyReset) == resetToEpoch(other.weeklyReset)
}

func (s aggregatedStatus) toPayload() statusPayload {
	return statusPayload{
		FiveHourUtilization: s.fiveHourUtilization,
		FiveHourReset:       resetToEpoch(s.fiveHourReset),
		WeeklyUtilization:   s.weeklyUtilization,
		WeeklyReset:         resetToEpoch(s.weeklyReset),
		PlanWeight:          s.totalWeight,
	}
}

func (s *Service) handleStatusEndpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, r, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	var provider credentialProvider
	var userConfig *option.OCMUser
	if len(s.options.Users) > 0 {
		if r.Header.Get("X-Api-Key") != "" || r.Header.Get("Api-Key") != "" {
			writeJSONError(w, r, http.StatusBadRequest, "invalid_request_error",
				"API key authentication is not supported; use Authorization: Bearer with an OCM user token")
			return
		}

		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			writeJSONError(w, r, http.StatusUnauthorized, "authentication_error", "missing api key")
			return
		}
		clientToken := strings.TrimPrefix(authHeader, "Bearer ")
		if clientToken == authHeader {
			writeJSONError(w, r, http.StatusUnauthorized, "authentication_error", "invalid api key format")
			return
		}
		username, ok := s.userManager.Authenticate(clientToken)
		if !ok {
			writeJSONError(w, r, http.StatusUnauthorized, "authentication_error", "invalid api key")
			return
		}

		userConfig = s.userConfigMap[username]
		var err error
		provider, err = credentialForUser(s.userConfigMap, s.providers, username)
		if err != nil {
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

	if r.URL.Query().Get("watch") == "true" {
		s.handleStatusStream(w, r, provider, userConfig)
		return
	}

	provider.pollIfStale(r.Context())
	status := s.computeAggregatedUtilization(provider, userConfig)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(status.toPayload())
}

func (s *Service) handleStatusStream(w http.ResponseWriter, r *http.Request, provider credentialProvider, userConfig *option.OCMUser) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, r, http.StatusInternalServerError, "api_error", "streaming not supported")
		return
	}

	subscription, done, err := s.statusObserver.Subscribe()
	if err != nil {
		writeJSONError(w, r, http.StatusInternalServerError, "api_error", "service closing")
		return
	}
	defer s.statusObserver.UnSubscribe(subscription)

	provider.pollIfStale(r.Context())

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	last := s.computeAggregatedUtilization(provider, userConfig)
	buf := &bytes.Buffer{}
	json.NewEncoder(buf).Encode(last.toPayload())
	_, writeErr := w.Write(buf.Bytes())
	if writeErr != nil {
		return
	}
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-done:
			return
		case <-subscription:
			for {
				select {
				case <-subscription:
				default:
					goto drained
				}
			}
		drained:
			current := s.computeAggregatedUtilization(provider, userConfig)
			if current.equal(last) {
				continue
			}
			last = current
			buf.Reset()
			json.NewEncoder(buf).Encode(current.toPayload())
			_, writeErr = w.Write(buf.Bytes())
			if writeErr != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Service) computeAggregatedUtilization(provider credentialProvider, userConfig *option.OCMUser) aggregatedStatus {
	var totalWeightedRemaining5h, totalWeightedRemainingWeekly, totalWeight float64
	now := time.Now()
	var totalWeightedHoursUntil5hReset, total5hResetWeight float64
	var totalWeightedHoursUntilWeeklyReset, totalWeeklyResetWeight float64
	for _, credential := range provider.allCredentials() {
		if !credential.isAvailable() {
			continue
		}
		if userConfig != nil && userConfig.ExternalCredential != "" && credential.tagName() == userConfig.ExternalCredential {
			continue
		}
		if userConfig != nil && !userConfig.AllowExternalUsage && credential.isExternal() {
			continue
		}
		weight := credential.planWeight()
		remaining5h := credential.fiveHourCap() - credential.fiveHourUtilization()
		if remaining5h < 0 {
			remaining5h = 0
		}
		remainingWeekly := credential.weeklyCap() - credential.weeklyUtilization()
		if remainingWeekly < 0 {
			remainingWeekly = 0
		}
		totalWeightedRemaining5h += remaining5h * weight
		totalWeightedRemainingWeekly += remainingWeekly * weight
		totalWeight += weight

		fiveHourReset := credential.fiveHourResetTime()
		if !fiveHourReset.IsZero() {
			hours := fiveHourReset.Sub(now).Hours()
			if hours > 0 {
				totalWeightedHoursUntil5hReset += hours * weight
				total5hResetWeight += weight
			}
		}
		weeklyReset := credential.weeklyResetTime()
		if !weeklyReset.IsZero() {
			hours := weeklyReset.Sub(now).Hours()
			if hours > 0 {
				totalWeightedHoursUntilWeeklyReset += hours * weight
				totalWeeklyResetWeight += weight
			}
		}
	}
	if totalWeight == 0 {
		return aggregatedStatus{
			fiveHourUtilization: 100,
			weeklyUtilization:   100,
		}
	}
	result := aggregatedStatus{
		fiveHourUtilization: 100 - totalWeightedRemaining5h/totalWeight,
		weeklyUtilization:   100 - totalWeightedRemainingWeekly/totalWeight,
		totalWeight:         totalWeight,
	}
	if total5hResetWeight > 0 {
		avgHours := totalWeightedHoursUntil5hReset / total5hResetWeight
		result.fiveHourReset = now.Add(time.Duration(avgHours * float64(time.Hour)))
	}
	if totalWeeklyResetWeight > 0 {
		avgHours := totalWeightedHoursUntilWeeklyReset / totalWeeklyResetWeight
		result.weeklyReset = now.Add(time.Duration(avgHours * float64(time.Hour)))
	}
	return result
}

func (s *Service) rewriteResponseHeaders(headers http.Header, provider credentialProvider, userConfig *option.OCMUser) {
	status := s.computeAggregatedUtilization(provider, userConfig)
	headers.Set("x-codex-primary-used-percent", strconv.FormatFloat(status.fiveHourUtilization, 'f', 2, 64))
	headers.Set("x-codex-secondary-used-percent", strconv.FormatFloat(status.weeklyUtilization, 'f', 2, 64))
	if !status.fiveHourReset.IsZero() {
		headers.Set("x-codex-primary-reset-at", strconv.FormatInt(status.fiveHourReset.Unix(), 10))
	} else {
		headers.Del("x-codex-primary-reset-at")
	}
	if !status.weeklyReset.IsZero() {
		headers.Set("x-codex-secondary-reset-at", strconv.FormatInt(status.weeklyReset.Unix(), 10))
	} else {
		headers.Del("x-codex-secondary-reset-at")
	}
	if status.totalWeight > 0 {
		headers.Set("X-OCM-Plan-Weight", strconv.FormatFloat(status.totalWeight, 'f', -1, 64))
	}
	rateLimitSuffixes := [...]string{
		"-primary-used-percent",
		"-primary-reset-at",
		"-secondary-used-percent",
		"-secondary-reset-at",
		"-secondary-window-minutes",
		"-limit-name",
	}
	for key := range headers {
		lowerKey := strings.ToLower(key)
		if !strings.HasPrefix(lowerKey, "x-") {
			continue
		}
		for _, suffix := range rateLimitSuffixes {
			if strings.HasSuffix(lowerKey, suffix) {
				if strings.TrimSuffix(lowerKey, suffix) != "x-codex" {
					headers.Del(key)
				}
				break
			}
		}
	}
}
