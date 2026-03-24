package ccm

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/sagernet/sing-box/option"
)

type statusPayload struct {
	FiveHourUtilization   float64              `json:"five_hour_utilization"`
	FiveHourReset         int64                `json:"five_hour_reset"`
	WeeklyUtilization     float64              `json:"weekly_utilization"`
	WeeklyReset           int64                `json:"weekly_reset"`
	PlanWeight            float64              `json:"plan_weight"`
	UnifiedStatus         string               `json:"unified_status,omitempty"`
	UnifiedReset          int64                `json:"unified_reset,omitempty"`
	RepresentativeClaim   string               `json:"representative_claim,omitempty"`
	FallbackAvailable     bool                 `json:"fallback_available,omitempty"`
	OverageStatus         string               `json:"overage_status,omitempty"`
	OverageReset          int64                `json:"overage_reset,omitempty"`
	OverageDisabledReason string               `json:"overage_disabled_reason,omitempty"`
	Availability          *availabilityPayload `json:"availability,omitempty"`
}

type aggregatedStatus struct {
	fiveHourUtilization float64
	weeklyUtilization   float64
	totalWeight         float64
	fiveHourReset       time.Time
	weeklyReset         time.Time
	unifiedRateLimit    unifiedRateLimitInfo
	availability        availabilityStatus
}

func resetToEpoch(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func (s aggregatedStatus) equal(other aggregatedStatus) bool {
	return reflect.DeepEqual(s.toPayload(), other.toPayload())
}

func (s aggregatedStatus) toPayload() statusPayload {
	unified := s.unifiedRateLimit.normalized()
	return statusPayload{
		FiveHourUtilization:   s.fiveHourUtilization,
		FiveHourReset:         resetToEpoch(s.fiveHourReset),
		WeeklyUtilization:     s.weeklyUtilization,
		WeeklyReset:           resetToEpoch(s.weeklyReset),
		PlanWeight:            s.totalWeight,
		UnifiedStatus:         string(unified.Status),
		UnifiedReset:          resetToEpoch(unified.ResetAt),
		RepresentativeClaim:   unified.RepresentativeClaim,
		FallbackAvailable:     unified.FallbackAvailable,
		OverageStatus:         unified.OverageStatus,
		OverageReset:          resetToEpoch(unified.OverageResetAt),
		OverageDisabledReason: unified.OverageDisabledReason,
		Availability:          s.availability.toPayload(),
	}
}

type aggregateInput struct {
	availability availabilityStatus
	unified      unifiedRateLimitInfo
}

func aggregateAvailability(inputs []aggregateInput) availabilityStatus {
	if len(inputs) == 0 {
		return availabilityStatus{
			State:  availabilityStateUnavailable,
			Reason: availabilityReasonNoCredentials,
		}
	}
	var earliestRateLimit time.Time
	var hasRateLimited bool
	var blocked availabilityStatus
	var hasBlocked bool
	var hasUnavailable bool
	for _, input := range inputs {
		availability := input.availability.normalized()
		switch availability.State {
		case availabilityStateUsable:
			return availabilityStatus{State: availabilityStateUsable}
		case availabilityStateRateLimited:
			hasRateLimited = true
			if !availability.ResetAt.IsZero() && (earliestRateLimit.IsZero() || availability.ResetAt.Before(earliestRateLimit)) {
				earliestRateLimit = availability.ResetAt
			}
			if blocked.State == "" {
				blocked = availabilityStatus{
					State:   availabilityStateRateLimited,
					Reason:  availabilityReasonHardRateLimit,
					ResetAt: earliestRateLimit,
				}
			}
		case availabilityStateTemporarilyBlocked:
			if !hasBlocked {
				blocked = availability
				hasBlocked = true
			}
			if !availability.ResetAt.IsZero() && (blocked.ResetAt.IsZero() || availability.ResetAt.Before(blocked.ResetAt)) {
				blocked.ResetAt = availability.ResetAt
			}
		case availabilityStateUnavailable:
			hasUnavailable = true
		}
	}
	if hasRateLimited {
		blocked.ResetAt = earliestRateLimit
		return blocked
	}
	if hasBlocked {
		return blocked
	}
	if hasUnavailable {
		return availabilityStatus{
			State:  availabilityStateUnavailable,
			Reason: availabilityReasonUnknown,
		}
	}
	return availabilityStatus{
		State:  availabilityStateUnknown,
		Reason: availabilityReasonUnknown,
	}
}

func chooseRepresentativeClaim(status unifiedRateLimitStatus, fiveHourUtilization float64, fiveHourReset time.Time, weeklyUtilization float64, weeklyReset time.Time, now time.Time) string {
	type claimCandidate struct {
		name        string
		priority    int
		utilization float64
	}
	candidateFor := func(name string, utilization float64, warning bool) claimCandidate {
		priority := 0
		switch {
		case status == unifiedRateLimitStatusRejected && utilization >= 100:
			priority = 2
		case warning:
			priority = 1
		}
		return claimCandidate{name: name, priority: priority, utilization: utilization}
	}
	five := candidateFor("5h", fiveHourUtilization, claudeFiveHourWarning(fiveHourUtilization, fiveHourReset, now))
	weekly := candidateFor("7d", weeklyUtilization, claudeWeeklyWarning(weeklyUtilization, weeklyReset, now))
	switch {
	case five.priority > weekly.priority:
		return five.name
	case weekly.priority > five.priority:
		return weekly.name
	case five.utilization > weekly.utilization:
		return five.name
	case weekly.utilization > five.utilization:
		return weekly.name
	case !fiveHourReset.IsZero():
		return five.name
	case !weeklyReset.IsZero():
		return weekly.name
	default:
		return "5h"
	}
}

func aggregateUnifiedRateLimit(inputs []aggregateInput, fiveHourUtilization float64, fiveHourReset time.Time, weeklyUtilization float64, weeklyReset time.Time, availability availabilityStatus) unifiedRateLimitInfo {
	now := time.Now()
	info := unifiedRateLimitInfo{}
	usableCount := 0
	for _, input := range inputs {
		if input.availability.State == availabilityStateUsable {
			usableCount++
		}
		if input.unified.OverageStatus != "" && info.OverageStatus == "" {
			info.OverageStatus = input.unified.OverageStatus
			info.OverageResetAt = input.unified.OverageResetAt
			info.OverageDisabledReason = input.unified.OverageDisabledReason
		}
		if input.unified.Status == unifiedRateLimitStatusRejected {
			info.Status = unifiedRateLimitStatusRejected
			if !input.unified.ResetAt.IsZero() && (info.ResetAt.IsZero() || input.unified.ResetAt.Before(info.ResetAt)) {
				info.ResetAt = input.unified.ResetAt
				info.RepresentativeClaim = input.unified.RepresentativeClaim
			}
		}
	}
	if info.Status == "" {
		switch {
		case availability.State == availabilityStateRateLimited || fiveHourUtilization >= 100 || weeklyUtilization >= 100:
			info.Status = unifiedRateLimitStatusRejected
			info.ResetAt = availability.ResetAt
		case claudeFiveHourWarning(fiveHourUtilization, fiveHourReset, now) || claudeWeeklyWarning(weeklyUtilization, weeklyReset, now):
			info.Status = unifiedRateLimitStatusAllowedWarning
		default:
			info.Status = unifiedRateLimitStatusAllowed
		}
	}
	info.FallbackAvailable = usableCount > 0 && len(inputs) > 1
	if info.RepresentativeClaim == "" {
		info.RepresentativeClaim = chooseRepresentativeClaim(info.Status, fiveHourUtilization, fiveHourReset, weeklyUtilization, weeklyReset, now)
	}
	if info.ResetAt.IsZero() {
		switch info.RepresentativeClaim {
		case "7d":
			info.ResetAt = weeklyReset
		default:
			info.ResetAt = fiveHourReset
		}
	}
	return info.normalized()
}

func (s *Service) handleStatusEndpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, r, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	var provider credentialProvider
	var userConfig *option.CCMUser
	if len(s.options.Users) > 0 {
		if r.Header.Get("X-Api-Key") != "" || r.Header.Get("Api-Key") != "" {
			writeJSONError(w, r, http.StatusBadRequest, "invalid_request_error",
				"API key authentication is not supported; use Authorization: Bearer with a CCM user token")
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

	provider.pollIfStale()
	status := s.computeAggregatedUtilization(provider, userConfig)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(status.toPayload())
}

func (s *Service) handleStatusStream(w http.ResponseWriter, r *http.Request, provider credentialProvider, userConfig *option.CCMUser) {
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

	provider.pollIfStale()

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

func (s *Service) computeAggregatedUtilization(provider credentialProvider, userConfig *option.CCMUser) aggregatedStatus {
	visibleInputs := make([]aggregateInput, 0, len(provider.allCredentials()))
	var totalWeightedRemaining5h, totalWeightedRemainingWeekly, totalWeight float64
	now := time.Now()
	var totalWeightedHoursUntil5hReset, total5hResetWeight float64
	var totalWeightedHoursUntilWeeklyReset, totalWeeklyResetWeight float64
	var hasSnapshotData bool
	for _, credential := range provider.allCredentials() {
		if userConfig != nil && userConfig.ExternalCredential != "" && credential.tagName() == userConfig.ExternalCredential {
			continue
		}
		if userConfig != nil && !userConfig.AllowExternalUsage && credential.isExternal() {
			continue
		}
		visibleInputs = append(visibleInputs, aggregateInput{
			availability: credential.availabilityStatus(),
			unified:      credential.unifiedRateLimitState(),
		})
		if !credential.hasSnapshotData() {
			continue
		}
		hasSnapshotData = true
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
	availability := aggregateAvailability(visibleInputs)
	if totalWeight == 0 {
		result := aggregatedStatus{availability: availability}
		if !hasSnapshotData {
			result.fiveHourUtilization = 100
			result.weeklyUtilization = 100
		}
		result.unifiedRateLimit = aggregateUnifiedRateLimit(visibleInputs, result.fiveHourUtilization, result.fiveHourReset, result.weeklyUtilization, result.weeklyReset, availability)
		return result
	}
	result := aggregatedStatus{
		fiveHourUtilization: 100 - totalWeightedRemaining5h/totalWeight,
		weeklyUtilization:   100 - totalWeightedRemainingWeekly/totalWeight,
		totalWeight:         totalWeight,
		availability:        availability,
	}
	if total5hResetWeight > 0 {
		avgHours := totalWeightedHoursUntil5hReset / total5hResetWeight
		result.fiveHourReset = now.Add(time.Duration(avgHours * float64(time.Hour)))
	}
	if totalWeeklyResetWeight > 0 {
		avgHours := totalWeightedHoursUntilWeeklyReset / totalWeeklyResetWeight
		result.weeklyReset = now.Add(time.Duration(avgHours * float64(time.Hour)))
	}
	result.unifiedRateLimit = aggregateUnifiedRateLimit(visibleInputs, result.fiveHourUtilization, result.fiveHourReset, result.weeklyUtilization, result.weeklyReset, availability)
	return result
}

func (s *Service) rewriteResponseHeaders(headers http.Header, provider credentialProvider, userConfig *option.CCMUser) {
	status := s.computeAggregatedUtilization(provider, userConfig)
	headers.Set("anthropic-ratelimit-unified-5h-utilization", strconv.FormatFloat(status.fiveHourUtilization/100, 'f', 6, 64))
	headers.Set("anthropic-ratelimit-unified-7d-utilization", strconv.FormatFloat(status.weeklyUtilization/100, 'f', 6, 64))
	if !status.fiveHourReset.IsZero() {
		headers.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(status.fiveHourReset.Unix(), 10))
	} else {
		headers.Del("anthropic-ratelimit-unified-5h-reset")
	}
	if !status.weeklyReset.IsZero() {
		headers.Set("anthropic-ratelimit-unified-7d-reset", strconv.FormatInt(status.weeklyReset.Unix(), 10))
	} else {
		headers.Del("anthropic-ratelimit-unified-7d-reset")
	}
	if status.totalWeight > 0 {
		headers.Set("X-CCM-Plan-Weight", strconv.FormatFloat(status.totalWeight, 'f', -1, 64))
	}
	headers.Set("anthropic-ratelimit-unified-status", string(status.unifiedRateLimit.normalized().Status))
	if !status.unifiedRateLimit.ResetAt.IsZero() {
		headers.Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(status.unifiedRateLimit.ResetAt.Unix(), 10))
	} else {
		headers.Del("anthropic-ratelimit-unified-reset")
	}
	if status.unifiedRateLimit.RepresentativeClaim != "" {
		headers.Set("anthropic-ratelimit-unified-representative-claim", status.unifiedRateLimit.RepresentativeClaim)
	} else {
		headers.Del("anthropic-ratelimit-unified-representative-claim")
	}
	if status.unifiedRateLimit.FallbackAvailable {
		headers.Set("anthropic-ratelimit-unified-fallback", "available")
	} else {
		headers.Del("anthropic-ratelimit-unified-fallback")
	}
	if status.unifiedRateLimit.OverageStatus != "" {
		headers.Set("anthropic-ratelimit-unified-overage-status", status.unifiedRateLimit.OverageStatus)
	} else {
		headers.Del("anthropic-ratelimit-unified-overage-status")
	}
	if !status.unifiedRateLimit.OverageResetAt.IsZero() {
		headers.Set("anthropic-ratelimit-unified-overage-reset", strconv.FormatInt(status.unifiedRateLimit.OverageResetAt.Unix(), 10))
	} else {
		headers.Del("anthropic-ratelimit-unified-overage-reset")
	}
	if status.unifiedRateLimit.OverageDisabledReason != "" {
		headers.Set("anthropic-ratelimit-unified-overage-disabled-reason", status.unifiedRateLimit.OverageDisabledReason)
	} else {
		headers.Del("anthropic-ratelimit-unified-overage-disabled-reason")
	}
	if claudeFiveHourWarning(status.fiveHourUtilization, status.fiveHourReset, time.Now()) || status.fiveHourUtilization >= 100 {
		headers.Set("anthropic-ratelimit-unified-5h-surpassed-threshold", "true")
	} else {
		headers.Del("anthropic-ratelimit-unified-5h-surpassed-threshold")
	}
	if claudeWeeklyWarning(status.weeklyUtilization, status.weeklyReset, time.Now()) || status.weeklyUtilization >= 100 {
		headers.Set("anthropic-ratelimit-unified-7d-surpassed-threshold", "true")
	} else {
		headers.Del("anthropic-ratelimit-unified-7d-surpassed-threshold")
	}
}
