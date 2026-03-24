package ccm

import "time"

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
	if s.State == "" {
		return nil
	}
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

type unifiedRateLimitStatus string

const (
	unifiedRateLimitStatusAllowed        unifiedRateLimitStatus = "allowed"
	unifiedRateLimitStatusAllowedWarning unifiedRateLimitStatus = "allowed_warning"
	unifiedRateLimitStatusRejected       unifiedRateLimitStatus = "rejected"
)

type unifiedRateLimitInfo struct {
	Status                unifiedRateLimitStatus
	ResetAt               time.Time
	RepresentativeClaim   string
	FallbackAvailable     bool
	OverageStatus         string
	OverageResetAt        time.Time
	OverageDisabledReason string
}

func (s unifiedRateLimitInfo) normalized() unifiedRateLimitInfo {
	if s.Status == "" {
		s.Status = unifiedRateLimitStatusAllowed
	}
	return s
}

func claudeWindowProgress(resetAt time.Time, windowSeconds float64, now time.Time) float64 {
	if resetAt.IsZero() || windowSeconds <= 0 {
		return 0
	}
	windowStart := resetAt.Add(-time.Duration(windowSeconds * float64(time.Second)))
	if now.Before(windowStart) {
		return 0
	}
	progress := now.Sub(windowStart).Seconds() / windowSeconds
	if progress < 0 {
		return 0
	}
	if progress > 1 {
		return 1
	}
	return progress
}

func claudeFiveHourWarning(utilizationPercent float64, resetAt time.Time, now time.Time) bool {
	return utilizationPercent >= 90 && claudeWindowProgress(resetAt, 5*60*60, now) >= 0.72
}

func claudeWeeklyWarning(utilizationPercent float64, resetAt time.Time, now time.Time) bool {
	progress := claudeWindowProgress(resetAt, 7*24*60*60, now)
	switch {
	case utilizationPercent >= 75:
		return progress >= 0.60
	case utilizationPercent >= 50:
		return progress >= 0.35
	case utilizationPercent >= 25:
		return progress >= 0.15
	default:
		return false
	}
}
