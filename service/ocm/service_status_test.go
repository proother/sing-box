package ocm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/observable"
)

type testCredential struct {
	tag          string
	external     bool
	available    bool
	usable       bool
	hasData      bool
	fiveHour     float64
	weekly       float64
	fiveHourCapV float64
	weeklyCapV   float64
	weight       float64
	fiveReset    time.Time
	weeklyReset  time.Time
	availability availabilityStatus
	activeLimit  string
	snapshots    []rateLimitSnapshot
}

func (c *testCredential) tagName() string              { return c.tag }
func (c *testCredential) isAvailable() bool            { return c.available }
func (c *testCredential) isUsable() bool               { return c.usable }
func (c *testCredential) isExternal() bool             { return c.external }
func (c *testCredential) hasSnapshotData() bool        { return c.hasData }
func (c *testCredential) fiveHourUtilization() float64 { return c.fiveHour }
func (c *testCredential) weeklyUtilization() float64   { return c.weekly }
func (c *testCredential) fiveHourCap() float64         { return c.fiveHourCapV }
func (c *testCredential) weeklyCap() float64           { return c.weeklyCapV }
func (c *testCredential) planWeight() float64          { return c.weight }
func (c *testCredential) weeklyResetTime() time.Time   { return c.weeklyReset }
func (c *testCredential) fiveHourResetTime() time.Time { return c.fiveReset }
func (c *testCredential) markRateLimited(time.Time)    {}
func (c *testCredential) markUpstreamRejected()        {}
func (c *testCredential) markTemporarilyBlocked(reason availabilityReason, resetAt time.Time) {
	c.availability = availabilityStatus{State: availabilityStateTemporarilyBlocked, Reason: reason, ResetAt: resetAt}
}
func (c *testCredential) availabilityStatus() availabilityStatus { return c.availability }
func (c *testCredential) rateLimitSnapshots() []rateLimitSnapshot {
	return slicesCloneSnapshots(c.snapshots)
}
func (c *testCredential) activeLimitID() string           { return c.activeLimit }
func (c *testCredential) earliestReset() time.Time        { return c.fiveReset }
func (c *testCredential) unavailableError() error         { return nil }
func (c *testCredential) getAccessToken() (string, error) { return "", nil }
func (c *testCredential) buildProxyRequest(context.Context, *http.Request, []byte, http.Header) (*http.Request, error) {
	return nil, nil
}
func (c *testCredential) updateStateFromHeaders(http.Header)                           {}
func (c *testCredential) wrapRequestContext(context.Context) *credentialRequestContext { return nil }
func (c *testCredential) interruptConnections()                                        {}
func (c *testCredential) setOnBecameUnusable(func())                                   {}
func (c *testCredential) setStatusSubscriber(*observable.Subscriber[struct{}])         {}
func (c *testCredential) start() error                                                 { return nil }
func (c *testCredential) pollUsage()                                                   {}
func (c *testCredential) lastUpdatedTime() time.Time                                   { return time.Now() }
func (c *testCredential) pollBackoff(time.Duration) time.Duration                      { return 0 }
func (c *testCredential) usageTrackerOrNil() *AggregatedUsage                          { return nil }
func (c *testCredential) httpClient() *http.Client                                     { return nil }
func (c *testCredential) close()                                                       {}
func (c *testCredential) ocmDialer() N.Dialer                                          { return nil }
func (c *testCredential) ocmIsAPIKeyMode() bool                                        { return false }
func (c *testCredential) ocmGetAccountID() string                                      { return "" }
func (c *testCredential) ocmGetBaseURL() string                                        { return "" }

func slicesCloneSnapshots(snapshots []rateLimitSnapshot) []rateLimitSnapshot {
	if len(snapshots) == 0 {
		return nil
	}
	cloned := make([]rateLimitSnapshot, 0, len(snapshots))
	for _, snapshot := range snapshots {
		cloned = append(cloned, cloneRateLimitSnapshot(snapshot))
	}
	return cloned
}

type testProvider struct {
	credentials []Credential
}

func (p *testProvider) selectCredential(string, credentialSelection) (Credential, bool, error) {
	return nil, false, nil
}
func (p *testProvider) onRateLimited(string, Credential, time.Time, credentialSelection) Credential {
	return nil
}
func (p *testProvider) linkProviderInterrupt(Credential, credentialSelection, func()) func() bool {
	return func() bool { return true }
}
func (p *testProvider) pollIfStale()                     {}
func (p *testProvider) pollCredentialIfStale(Credential) {}
func (p *testProvider) allCredentials() []Credential     { return p.credentials }
func (p *testProvider) close()                           {}

func TestComputeAggregatedUtilizationPreservesStoredSnapshots(t *testing.T) {
	t.Parallel()

	service := &Service{}
	status := service.computeAggregatedUtilization(&testProvider{credentials: []Credential{
		&testCredential{
			tag:          "a",
			available:    true,
			usable:       false,
			hasData:      true,
			weight:       1,
			activeLimit:  "codex",
			availability: availabilityStatus{State: availabilityStateRateLimited, Reason: availabilityReasonHardRateLimit, ResetAt: time.Now().Add(time.Minute)},
			snapshots: []rateLimitSnapshot{
				{
					LimitID:   "codex",
					Primary:   &rateLimitWindow{UsedPercent: 44, WindowMinutes: 300, ResetAt: time.Now().Add(time.Hour).Unix()},
					Secondary: &rateLimitWindow{UsedPercent: 12, WindowMinutes: 10080, ResetAt: time.Now().Add(24 * time.Hour).Unix()},
				},
			},
		},
	}}, nil)

	if status.fiveHourUtilization != 44 || status.weeklyUtilization != 12 {
		t.Fatalf("expected stored snapshot utilization, got 5h=%v weekly=%v", status.fiveHourUtilization, status.weeklyUtilization)
	}
	if status.availability.State != availabilityStateRateLimited {
		t.Fatalf("expected rate-limited availability, got %#v", status.availability)
	}
}

func TestRewriteResponseHeadersIncludesAdditionalLimitFamiliesAndCredits(t *testing.T) {
	t.Parallel()

	service := &Service{}
	headers := make(http.Header)
	service.rewriteResponseHeaders(headers, &testProvider{credentials: []Credential{
		&testCredential{
			tag:          "a",
			available:    true,
			usable:       true,
			hasData:      true,
			weight:       1,
			activeLimit:  "codex_other",
			availability: availabilityStatus{State: availabilityStateUsable},
			snapshots: []rateLimitSnapshot{
				{
					LimitID:   "codex",
					Primary:   &rateLimitWindow{UsedPercent: 20, WindowMinutes: 300, ResetAt: time.Now().Add(time.Hour).Unix()},
					Secondary: &rateLimitWindow{UsedPercent: 40, WindowMinutes: 10080, ResetAt: time.Now().Add(24 * time.Hour).Unix()},
					Credits:   &creditsSnapshot{HasCredits: true, Unlimited: false, Balance: "12"},
				},
				{
					LimitID:   "codex_other",
					LimitName: "codex-other",
					Primary:   &rateLimitWindow{UsedPercent: 60, WindowMinutes: 60, ResetAt: time.Now().Add(30 * time.Minute).Unix()},
				},
			},
		},
	}}, nil)

	if headers.Get("x-codex-active-limit") != "codex-other" {
		t.Fatalf("expected active limit header, got %q", headers.Get("x-codex-active-limit"))
	}
	if headers.Get("x-codex-other-primary-used-percent") == "" {
		t.Fatal("expected additional rate-limit family header")
	}
	if headers.Get("x-codex-credits-balance") != "12" {
		t.Fatalf("expected credits balance header, got %q", headers.Get("x-codex-credits-balance"))
	}
}

func TestHandleWebSocketErrorEventConnectionLimitDoesNotUseRateLimitPath(t *testing.T) {
	t.Parallel()

	credential := &testCredential{availability: availabilityStatus{State: availabilityStateUsable}}
	service := &Service{}
	service.handleWebSocketErrorEvent([]byte(`{"type":"error","status_code":400,"error":{"code":"websocket_connection_limit_reached"}}`), credential)

	if credential.availability.State != availabilityStateTemporarilyBlocked || credential.availability.Reason != availabilityReasonConnectionLimit {
		t.Fatalf("expected temporary connection limit block, got %#v", credential.availability)
	}
}

func TestWriteCredentialUnavailableErrorReturns429ForRateLimitedCredentials(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	provider := &testProvider{credentials: []Credential{
		&testCredential{
			tag:          "a",
			available:    true,
			usable:       false,
			hasData:      true,
			weight:       1,
			availability: availabilityStatus{State: availabilityStateRateLimited, Reason: availabilityReasonHardRateLimit, ResetAt: time.Now().Add(time.Minute)},
			snapshots:    []rateLimitSnapshot{{LimitID: "codex", Primary: &rateLimitWindow{UsedPercent: 80}}},
		},
	}}

	writeCredentialUnavailableError(recorder, request, provider, provider.credentials[0], credentialSelection{}, "all credentials rate-limited")

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", recorder.Code)
	}
	var body map[string]map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"]["type"] != "usage_limit_reached" {
		t.Fatalf("expected usage_limit_reached type, got %#v", body)
	}
}
