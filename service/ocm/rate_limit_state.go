package ocm

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

type availabilityState string

const (
	availabilityStateUsable             availabilityState = "usable"
	availabilityStateRateLimited        availabilityState = "rate_limited"
	availabilityStateTemporarilyBlocked availabilityState = "temporarily_blocked"
	availabilityStateUnavailable        availabilityState = "unavailable"
	availabilityStateUnknown            availabilityState = "unknown"
)

type availabilityReason string

const (
	availabilityReasonHardRateLimit    availabilityReason = "hard_rate_limit"
	availabilityReasonConnectionLimit  availabilityReason = "connection_limit"
	availabilityReasonPollFailed       availabilityReason = "poll_failed"
	availabilityReasonUpstreamRejected availabilityReason = "upstream_rejected"
	availabilityReasonNoCredentials    availabilityReason = "no_credentials"
	availabilityReasonUnknown          availabilityReason = "unknown"
)

type availabilityStatus struct {
	State   availabilityState
	Reason  availabilityReason
	ResetAt time.Time
}

type availabilityPayload struct {
	State   string `json:"state"`
	Reason  string `json:"reason,omitempty"`
	ResetAt int64  `json:"reset_at,omitempty"`
}

func (s availabilityStatus) normalized() availabilityStatus {
	if s.State == "" {
		s.State = availabilityStateUnknown
	}
	if s.Reason == "" && s.State != availabilityStateUsable {
		s.Reason = availabilityReasonUnknown
	}
	return s
}

func (s availabilityStatus) toPayload() *availabilityPayload {
	s = s.normalized()
	payload := &availabilityPayload{
		State: string(s.State),
	}
	if s.Reason != "" && s.Reason != availabilityReasonUnknown {
		payload.Reason = string(s.Reason)
	}
	if !s.ResetAt.IsZero() {
		payload.ResetAt = s.ResetAt.Unix()
	}
	return payload
}

type creditsSnapshot struct {
	HasCredits bool   `json:"has_credits"`
	Unlimited  bool   `json:"unlimited"`
	Balance    string `json:"balance,omitempty"`
}

type rateLimitWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int64   `json:"window_minutes,omitempty"`
	ResetAt       int64   `json:"reset_at,omitempty"`
}

type rateLimitSnapshot struct {
	LimitID   string           `json:"limit_id,omitempty"`
	LimitName string           `json:"limit_name,omitempty"`
	Primary   *rateLimitWindow `json:"primary,omitempty"`
	Secondary *rateLimitWindow `json:"secondary,omitempty"`
	Credits   *creditsSnapshot `json:"credits,omitempty"`
	PlanType  string           `json:"plan_type,omitempty"`
}

func normalizeStoredLimitID(limitID string) string {
	normalized := normalizeRateLimitIdentifier(limitID)
	if normalized == "" {
		return ""
	}
	return strings.ReplaceAll(normalized, "-", "_")
}

func headerLimitID(limitID string) string {
	if limitID == "" {
		return "codex"
	}
	return strings.ReplaceAll(normalizeStoredLimitID(limitID), "_", "-")
}

func defaultRateLimitSnapshot(limitID string) rateLimitSnapshot {
	if limitID == "" {
		limitID = "codex"
	}
	return rateLimitSnapshot{LimitID: normalizeStoredLimitID(limitID)}
}

func cloneCreditsSnapshot(snapshot *creditsSnapshot) *creditsSnapshot {
	if snapshot == nil {
		return nil
	}
	cloned := *snapshot
	return &cloned
}

func cloneRateLimitWindow(window *rateLimitWindow) *rateLimitWindow {
	if window == nil {
		return nil
	}
	cloned := *window
	return &cloned
}

func cloneRateLimitSnapshot(snapshot rateLimitSnapshot) rateLimitSnapshot {
	snapshot.Primary = cloneRateLimitWindow(snapshot.Primary)
	snapshot.Secondary = cloneRateLimitWindow(snapshot.Secondary)
	snapshot.Credits = cloneCreditsSnapshot(snapshot.Credits)
	return snapshot
}

func sortRateLimitSnapshots(snapshots []rateLimitSnapshot) {
	slices.SortFunc(snapshots, func(a, b rateLimitSnapshot) int {
		return strings.Compare(a.LimitID, b.LimitID)
	})
}

func parseHeaderFloat(headers http.Header, name string) (float64, bool) {
	value := strings.TrimSpace(headers.Get(name))
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, false
	}
	if !isFinite(parsed) {
		return 0, false
	}
	return parsed, true
}

func isFinite(value float64) bool {
	return !((value != value) || value > 1e308 || value < -1e308)
}

func parseCreditsSnapshotFromHeaders(headers http.Header) *creditsSnapshot {
	hasCreditsValue := strings.TrimSpace(headers.Get("x-codex-credits-has-credits"))
	unlimitedValue := strings.TrimSpace(headers.Get("x-codex-credits-unlimited"))
	if hasCreditsValue == "" || unlimitedValue == "" {
		return nil
	}
	hasCredits := strings.EqualFold(hasCreditsValue, "true") || hasCreditsValue == "1"
	unlimited := strings.EqualFold(unlimitedValue, "true") || unlimitedValue == "1"
	return &creditsSnapshot{
		HasCredits: hasCredits,
		Unlimited:  unlimited,
		Balance:    strings.TrimSpace(headers.Get("x-codex-credits-balance")),
	}
}

func parseRateLimitWindowFromHeaders(headers http.Header, prefix string, windowName string) *rateLimitWindow {
	usedPercent, hasPercent := parseHeaderFloat(headers, prefix+"-"+windowName+"-used-percent")
	windowMinutes, hasWindow := parseInt64Header(headers, prefix+"-"+windowName+"-window-minutes")
	resetAt, hasReset := parseInt64Header(headers, prefix+"-"+windowName+"-reset-at")
	if !hasPercent && !hasWindow && !hasReset {
		return nil
	}
	window := &rateLimitWindow{}
	if hasPercent {
		window.UsedPercent = usedPercent
	}
	if hasWindow {
		window.WindowMinutes = windowMinutes
	}
	if hasReset {
		window.ResetAt = resetAt
	}
	return window
}

func parseRateLimitSnapshotsFromHeaders(headers http.Header) []rateLimitSnapshot {
	limitIDs := map[string]struct{}{}
	for key := range headers {
		lowerKey := strings.ToLower(key)
		if strings.HasPrefix(lowerKey, "x-") && strings.Contains(lowerKey, "-primary-") {
			limitID := strings.TrimPrefix(lowerKey, "x-")
			if suffix := strings.Index(limitID, "-primary-"); suffix > 0 {
				limitIDs[normalizeStoredLimitID(limitID[:suffix])] = struct{}{}
			}
		}
		if strings.HasPrefix(lowerKey, "x-") && strings.Contains(lowerKey, "-secondary-") {
			limitID := strings.TrimPrefix(lowerKey, "x-")
			if suffix := strings.Index(limitID, "-secondary-"); suffix > 0 {
				limitIDs[normalizeStoredLimitID(limitID[:suffix])] = struct{}{}
			}
		}
	}
	if activeLimit := normalizeStoredLimitID(headers.Get("x-codex-active-limit")); activeLimit != "" {
		limitIDs[activeLimit] = struct{}{}
	}
	if credits := parseCreditsSnapshotFromHeaders(headers); credits != nil {
		_ = credits
		limitIDs["codex"] = struct{}{}
	}
	if len(limitIDs) == 0 {
		return nil
	}
	snapshots := make([]rateLimitSnapshot, 0, len(limitIDs))
	for limitID := range limitIDs {
		prefix := "x-" + headerLimitID(limitID)
		snapshot := defaultRateLimitSnapshot(limitID)
		snapshot.LimitName = strings.TrimSpace(headers.Get(prefix + "-limit-name"))
		snapshot.Primary = parseRateLimitWindowFromHeaders(headers, prefix, "primary")
		snapshot.Secondary = parseRateLimitWindowFromHeaders(headers, prefix, "secondary")
		if limitID == "codex" {
			snapshot.Credits = parseCreditsSnapshotFromHeaders(headers)
		}
		if snapshot.Primary == nil && snapshot.Secondary == nil && snapshot.Credits == nil {
			continue
		}
		snapshots = append(snapshots, snapshot)
	}
	sortRateLimitSnapshots(snapshots)
	return snapshots
}

type usageRateLimitWindowPayload struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

type usageRateLimitDetailsPayload struct {
	PrimaryWindow   *usageRateLimitWindowPayload `json:"primary_window"`
	SecondaryWindow *usageRateLimitWindowPayload `json:"secondary_window"`
}

type usageCreditsPayload struct {
	HasCredits bool    `json:"has_credits"`
	Unlimited  bool    `json:"unlimited"`
	Balance    *string `json:"balance"`
}

type additionalRateLimitPayload struct {
	LimitName      string                        `json:"limit_name"`
	MeteredFeature string                        `json:"metered_feature"`
	RateLimit      *usageRateLimitDetailsPayload `json:"rate_limit"`
}

type usageRateLimitStatusPayload struct {
	PlanType             string                        `json:"plan_type"`
	RateLimit            *usageRateLimitDetailsPayload `json:"rate_limit"`
	Credits              *usageCreditsPayload          `json:"credits"`
	AdditionalRateLimits []additionalRateLimitPayload  `json:"additional_rate_limits"`
}

func windowFromUsagePayload(window *usageRateLimitWindowPayload) *rateLimitWindow {
	if window == nil {
		return nil
	}
	result := &rateLimitWindow{
		UsedPercent: window.UsedPercent,
	}
	if window.LimitWindowSeconds > 0 {
		result.WindowMinutes = (window.LimitWindowSeconds + 59) / 60
	}
	if window.ResetAt > 0 {
		result.ResetAt = window.ResetAt
	}
	return result
}

func snapshotsFromUsagePayload(payload usageRateLimitStatusPayload) []rateLimitSnapshot {
	snapshots := make([]rateLimitSnapshot, 0, 1+len(payload.AdditionalRateLimits))
	codex := defaultRateLimitSnapshot("codex")
	codex.PlanType = payload.PlanType
	if payload.RateLimit != nil {
		codex.Primary = windowFromUsagePayload(payload.RateLimit.PrimaryWindow)
		codex.Secondary = windowFromUsagePayload(payload.RateLimit.SecondaryWindow)
	}
	if payload.Credits != nil {
		codex.Credits = &creditsSnapshot{
			HasCredits: payload.Credits.HasCredits,
			Unlimited:  payload.Credits.Unlimited,
		}
		if payload.Credits.Balance != nil {
			codex.Credits.Balance = *payload.Credits.Balance
		}
	}
	if codex.Primary != nil || codex.Secondary != nil || codex.Credits != nil || codex.PlanType != "" {
		snapshots = append(snapshots, codex)
	}
	for _, additional := range payload.AdditionalRateLimits {
		snapshot := defaultRateLimitSnapshot(additional.MeteredFeature)
		snapshot.LimitName = additional.LimitName
		snapshot.PlanType = payload.PlanType
		if additional.RateLimit != nil {
			snapshot.Primary = windowFromUsagePayload(additional.RateLimit.PrimaryWindow)
			snapshot.Secondary = windowFromUsagePayload(additional.RateLimit.SecondaryWindow)
		}
		if snapshot.Primary == nil && snapshot.Secondary == nil {
			continue
		}
		snapshots = append(snapshots, snapshot)
	}
	sortRateLimitSnapshots(snapshots)
	return snapshots
}

func applyRateLimitSnapshotsLocked(state *credentialState, snapshots []rateLimitSnapshot, activeLimitID string, planWeight float64, planType string) {
	if len(snapshots) == 0 {
		return
	}
	if state.rateLimitSnapshots == nil {
		state.rateLimitSnapshots = make(map[string]rateLimitSnapshot, len(snapshots))
	} else {
		clear(state.rateLimitSnapshots)
	}
	for _, snapshot := range snapshots {
		snapshot = cloneRateLimitSnapshot(snapshot)
		if snapshot.LimitID == "" {
			snapshot.LimitID = "codex"
		}
		if snapshot.LimitName == "" && snapshot.LimitID != "codex" {
			snapshot.LimitName = strings.ReplaceAll(snapshot.LimitID, "_", "-")
		}
		if snapshot.PlanType == "" {
			snapshot.PlanType = planType
		}
		state.rateLimitSnapshots[snapshot.LimitID] = snapshot
	}
	if planWeight > 0 {
		state.remotePlanWeight = planWeight
	}
	if planType != "" {
		state.accountType = planType
	}
	if normalizedActive := normalizeStoredLimitID(activeLimitID); normalizedActive != "" {
		state.activeLimitID = normalizedActive
	} else if state.activeLimitID == "" {
		if _, exists := state.rateLimitSnapshots["codex"]; exists {
			state.activeLimitID = "codex"
		} else {
			for limitID := range state.rateLimitSnapshots {
				state.activeLimitID = limitID
				break
			}
		}
	}
	legacy := state.rateLimitSnapshots["codex"]
	if legacy.LimitID == "" && state.activeLimitID != "" {
		legacy = state.rateLimitSnapshots[state.activeLimitID]
	}
	state.fiveHourUtilization = 0
	state.fiveHourReset = time.Time{}
	state.weeklyUtilization = 0
	state.weeklyReset = time.Time{}
	if legacy.Primary != nil {
		state.fiveHourUtilization = legacy.Primary.UsedPercent
		if legacy.Primary.ResetAt > 0 {
			state.fiveHourReset = time.Unix(legacy.Primary.ResetAt, 0)
		}
	}
	if legacy.Secondary != nil {
		state.weeklyUtilization = legacy.Secondary.UsedPercent
		if legacy.Secondary.ResetAt > 0 {
			state.weeklyReset = time.Unix(legacy.Secondary.ResetAt, 0)
		}
	}
	state.noteSnapshotData()
}
