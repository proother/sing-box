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
