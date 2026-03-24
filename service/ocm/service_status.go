package ocm

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sagernet/sing-box/option"
)

type statusPayload struct {
	FiveHourUtilization float64              `json:"five_hour_utilization"`
	FiveHourReset       int64                `json:"five_hour_reset"`
	WeeklyUtilization   float64              `json:"weekly_utilization"`
	WeeklyReset         int64                `json:"weekly_reset"`
	PlanWeight          float64              `json:"plan_weight"`
	ActiveLimit         string               `json:"active_limit,omitempty"`
	Limits              []rateLimitSnapshot  `json:"limits,omitempty"`
	Availability        *availabilityPayload `json:"availability,omitempty"`
}

type aggregatedStatus struct {
	fiveHourUtilization float64
	weeklyUtilization   float64
	totalWeight         float64
	fiveHourReset       time.Time
	weeklyReset         time.Time
	activeLimitID       string
	limits              []rateLimitSnapshot
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
	return statusPayload{
		FiveHourUtilization: s.fiveHourUtilization,
		FiveHourReset:       resetToEpoch(s.fiveHourReset),
		WeeklyUtilization:   s.weeklyUtilization,
		WeeklyReset:         resetToEpoch(s.weeklyReset),
		PlanWeight:          s.totalWeight,
		ActiveLimit:         s.activeLimitID,
		Limits:              slices.Clone(s.limits),
		Availability:        s.availability.toPayload(),
	}
}

type aggregateInput struct {
	weight       float64
	snapshots    []rateLimitSnapshot
	activeLimit  string
	availability availabilityStatus
}

type snapshotContribution struct {
	weight   float64
	snapshot rateLimitSnapshot
}

func aggregateAvailability(inputs []aggregateInput) availabilityStatus {
	if len(inputs) == 0 {
		return availabilityStatus{
			State:  availabilityStateUnavailable,
			Reason: availabilityReasonNoCredentials,
		}
	}
	var earliestRateLimited time.Time
	var hasRateLimited bool
	var bestBlocked availabilityStatus
	var hasBlocked bool
	var hasUnavailable bool
	blockedPriority := func(reason availabilityReason) int {
		switch reason {
		case availabilityReasonConnectionLimit:
			return 3
		case availabilityReasonPollFailed:
			return 2
		case availabilityReasonUpstreamRejected:
			return 1
		default:
			return 0
		}
	}
	for _, input := range inputs {
		availability := input.availability.normalized()
		switch availability.State {
		case availabilityStateUsable:
			return availabilityStatus{State: availabilityStateUsable}
		case availabilityStateRateLimited:
			hasRateLimited = true
			if !availability.ResetAt.IsZero() && (earliestRateLimited.IsZero() || availability.ResetAt.Before(earliestRateLimited)) {
				earliestRateLimited = availability.ResetAt
			}
		case availabilityStateTemporarilyBlocked:
			if !hasBlocked || blockedPriority(availability.Reason) > blockedPriority(bestBlocked.Reason) {
				bestBlocked = availability
				hasBlocked = true
			}
			if hasBlocked && !availability.ResetAt.IsZero() && (bestBlocked.ResetAt.IsZero() || availability.ResetAt.Before(bestBlocked.ResetAt)) {
				bestBlocked.ResetAt = availability.ResetAt
			}
		case availabilityStateUnavailable:
			hasUnavailable = true
		}
	}
	if hasRateLimited {
		return availabilityStatus{
			State:   availabilityStateRateLimited,
			Reason:  availabilityReasonHardRateLimit,
			ResetAt: earliestRateLimited,
		}
	}
	if hasBlocked {
		return bestBlocked
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

func aggregateRateLimitWindow(contributions []snapshotContribution, selector func(rateLimitSnapshot) *rateLimitWindow) *rateLimitWindow {
	var totalWeight float64
	var totalRemaining float64
	var totalWindowMinutes float64
	var totalResetHours float64
	var resetWeight float64
	now := time.Now()
	for _, contribution := range contributions {
		window := selector(contribution.snapshot)
		if window == nil {
			continue
		}
		totalWeight += contribution.weight
		totalRemaining += (100 - window.UsedPercent) * contribution.weight
		if window.WindowMinutes > 0 {
			totalWindowMinutes += float64(window.WindowMinutes) * contribution.weight
		}
		if window.ResetAt > 0 {
			resetTime := time.Unix(window.ResetAt, 0)
			hours := resetTime.Sub(now).Hours()
			if hours > 0 {
				totalResetHours += hours * contribution.weight
				resetWeight += contribution.weight
			}
		}
	}
	if totalWeight == 0 {
		return nil
	}
	window := &rateLimitWindow{
		UsedPercent: 100 - totalRemaining/totalWeight,
	}
	if totalWindowMinutes > 0 {
		window.WindowMinutes = int64(totalWindowMinutes / totalWeight)
	}
	if resetWeight > 0 {
		window.ResetAt = now.Add(time.Duration(totalResetHours / resetWeight * float64(time.Hour))).Unix()
	}
	return window
}

func aggregateCredits(contributions []snapshotContribution) *creditsSnapshot {
	var hasCredits bool
	var unlimited bool
	var balanceTotal float64
	var hasBalance bool
	for _, contribution := range contributions {
		if contribution.snapshot.Credits == nil {
			continue
		}
		hasCredits = hasCredits || contribution.snapshot.Credits.HasCredits
		unlimited = unlimited || contribution.snapshot.Credits.Unlimited
		if balance := strings.TrimSpace(contribution.snapshot.Credits.Balance); balance != "" {
			value, err := strconv.ParseFloat(balance, 64)
			if err == nil {
				balanceTotal += value
				hasBalance = true
			}
		}
	}
	if !hasCredits && !unlimited && !hasBalance {
		return nil
	}
	credits := &creditsSnapshot{
		HasCredits: hasCredits,
		Unlimited:  unlimited,
	}
	if hasBalance && !unlimited {
		credits.Balance = strconv.FormatFloat(balanceTotal, 'f', -1, 64)
	}
	return credits
}

func aggregateSnapshots(inputs []aggregateInput) []rateLimitSnapshot {
	grouped := make(map[string][]snapshotContribution)
	for _, input := range inputs {
		for _, snapshot := range input.snapshots {
			limitID := snapshot.LimitID
			if limitID == "" {
				limitID = "codex"
			}
			grouped[limitID] = append(grouped[limitID], snapshotContribution{
				weight:   input.weight,
				snapshot: snapshot,
			})
		}
	}
	if len(grouped) == 0 {
		return nil
	}
	aggregated := make([]rateLimitSnapshot, 0, len(grouped))
	for limitID, contributions := range grouped {
		snapshot := defaultRateLimitSnapshot(limitID)
		var bestPlanWeight float64
		for _, contribution := range contributions {
			if contribution.snapshot.LimitName != "" && snapshot.LimitName == "" {
				snapshot.LimitName = contribution.snapshot.LimitName
			}
			if contribution.snapshot.PlanType != "" && contribution.weight >= bestPlanWeight {
				bestPlanWeight = contribution.weight
				snapshot.PlanType = contribution.snapshot.PlanType
			}
		}
		snapshot.Primary = aggregateRateLimitWindow(contributions, func(snapshot rateLimitSnapshot) *rateLimitWindow {
			return snapshot.Primary
		})
		snapshot.Secondary = aggregateRateLimitWindow(contributions, func(snapshot rateLimitSnapshot) *rateLimitWindow {
			return snapshot.Secondary
		})
		snapshot.Credits = aggregateCredits(contributions)
		if snapshot.Primary == nil && snapshot.Secondary == nil && snapshot.Credits == nil {
			continue
		}
		aggregated = append(aggregated, snapshot)
	}
	sortRateLimitSnapshots(aggregated)
	return aggregated
}

func selectActiveLimitID(inputs []aggregateInput, snapshots []rateLimitSnapshot) string {
	if len(snapshots) == 0 {
		return ""
	}
	weights := make(map[string]float64)
	for _, input := range inputs {
		if input.activeLimit == "" {
			continue
		}
		weights[normalizeStoredLimitID(input.activeLimit)] += input.weight
	}
	var (
		bestID     string
		bestWeight float64
	)
	for limitID, weight := range weights {
		if weight > bestWeight {
			bestID = limitID
			bestWeight = weight
		}
	}
	if bestID != "" {
		return bestID
	}
	for _, snapshot := range snapshots {
		if snapshot.LimitID == "codex" {
			return "codex"
		}
	}
	return snapshots[0].LimitID
}

func findSnapshotByLimitID(snapshots []rateLimitSnapshot, limitID string) *rateLimitSnapshot {
	for _, snapshot := range snapshots {
		if snapshot.LimitID == limitID {
			snapshotCopy := snapshot
			return &snapshotCopy
		}
	}
	return nil
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

	provider.pollIfStale()
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

func (s *Service) computeAggregatedUtilization(provider credentialProvider, userConfig *option.OCMUser) aggregatedStatus {
	inputs := make([]aggregateInput, 0, len(provider.allCredentials()))
	var totalWeight float64
	var hasSnapshotData bool
	for _, credential := range provider.allCredentials() {
		if userConfig != nil && userConfig.ExternalCredential != "" && credential.tagName() == userConfig.ExternalCredential {
			continue
		}
		if userConfig != nil && !userConfig.AllowExternalUsage && credential.isExternal() {
			continue
		}
		input := aggregateInput{
			weight:       credential.planWeight(),
			snapshots:    credential.rateLimitSnapshots(),
			activeLimit:  credential.activeLimitID(),
			availability: credential.availabilityStatus(),
		}
		inputs = append(inputs, input)
		if credential.hasSnapshotData() {
			hasSnapshotData = true
		}
		totalWeight += input.weight
	}
	limits := aggregateSnapshots(inputs)
	result := aggregatedStatus{
		totalWeight:   totalWeight,
		availability:  aggregateAvailability(inputs),
		limits:        limits,
		activeLimitID: selectActiveLimitID(inputs, limits),
	}
	if legacy := findSnapshotByLimitID(result.limits, "codex"); legacy != nil {
		if legacy.Primary != nil {
			result.fiveHourUtilization = legacy.Primary.UsedPercent
			if legacy.Primary.ResetAt > 0 {
				result.fiveHourReset = time.Unix(legacy.Primary.ResetAt, 0)
			}
		}
		if legacy.Secondary != nil {
			result.weeklyUtilization = legacy.Secondary.UsedPercent
			if legacy.Secondary.ResetAt > 0 {
				result.weeklyReset = time.Unix(legacy.Secondary.ResetAt, 0)
			}
		}
	} else if legacy := findSnapshotByLimitID(result.limits, result.activeLimitID); legacy != nil {
		if legacy.Primary != nil {
			result.fiveHourUtilization = legacy.Primary.UsedPercent
			if legacy.Primary.ResetAt > 0 {
				result.fiveHourReset = time.Unix(legacy.Primary.ResetAt, 0)
			}
		}
		if legacy.Secondary != nil {
			result.weeklyUtilization = legacy.Secondary.UsedPercent
			if legacy.Secondary.ResetAt > 0 {
				result.weeklyReset = time.Unix(legacy.Secondary.ResetAt, 0)
			}
		}
	}
	if len(result.limits) == 0 && !hasSnapshotData {
		result.fiveHourUtilization = 100
		result.weeklyUtilization = 100
	}
	return result
}

func (s *Service) rewriteResponseHeaders(headers http.Header, provider credentialProvider, userConfig *option.OCMUser) {
	status := s.computeAggregatedUtilization(provider, userConfig)
	for key := range headers {
		lowerKey := strings.ToLower(key)
		if lowerKey == "x-codex-active-limit" ||
			strings.HasSuffix(lowerKey, "-primary-used-percent") ||
			strings.HasSuffix(lowerKey, "-primary-window-minutes") ||
			strings.HasSuffix(lowerKey, "-primary-reset-at") ||
			strings.HasSuffix(lowerKey, "-secondary-used-percent") ||
			strings.HasSuffix(lowerKey, "-secondary-window-minutes") ||
			strings.HasSuffix(lowerKey, "-secondary-reset-at") ||
			strings.HasSuffix(lowerKey, "-limit-name") ||
			strings.HasPrefix(lowerKey, "x-codex-credits-") {
			headers.Del(key)
		}
	}
	headers.Set("x-codex-active-limit", headerLimitID(status.activeLimitID))
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
	for _, snapshot := range status.limits {
		prefix := "x-" + headerLimitID(snapshot.LimitID)
		if snapshot.Primary != nil {
			headers.Set(prefix+"-primary-used-percent", strconv.FormatFloat(snapshot.Primary.UsedPercent, 'f', 2, 64))
			if snapshot.Primary.WindowMinutes > 0 {
				headers.Set(prefix+"-primary-window-minutes", strconv.FormatInt(snapshot.Primary.WindowMinutes, 10))
			}
			if snapshot.Primary.ResetAt > 0 {
				headers.Set(prefix+"-primary-reset-at", strconv.FormatInt(snapshot.Primary.ResetAt, 10))
			}
		}
		if snapshot.Secondary != nil {
			headers.Set(prefix+"-secondary-used-percent", strconv.FormatFloat(snapshot.Secondary.UsedPercent, 'f', 2, 64))
			if snapshot.Secondary.WindowMinutes > 0 {
				headers.Set(prefix+"-secondary-window-minutes", strconv.FormatInt(snapshot.Secondary.WindowMinutes, 10))
			}
			if snapshot.Secondary.ResetAt > 0 {
				headers.Set(prefix+"-secondary-reset-at", strconv.FormatInt(snapshot.Secondary.ResetAt, 10))
			}
		}
		if snapshot.LimitName != "" {
			headers.Set(prefix+"-limit-name", snapshot.LimitName)
		}
		if snapshot.LimitID == "codex" && snapshot.Credits != nil {
			headers.Set("x-codex-credits-has-credits", strconv.FormatBool(snapshot.Credits.HasCredits))
			headers.Set("x-codex-credits-unlimited", strconv.FormatBool(snapshot.Credits.Unlimited))
			if snapshot.Credits.Balance != "" {
				headers.Set("x-codex-credits-balance", snapshot.Credits.Balance)
			}
		}
	}
}
