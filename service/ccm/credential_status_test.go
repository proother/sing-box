package ccm

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/observable"

	"github.com/hashicorp/yamux"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func drainStatusEvents(subscription observable.Subscription[struct{}]) int {
	var count int
	for {
		select {
		case <-subscription:
			count++
		default:
			return count
		}
	}
}

func newTestLogger() log.ContextLogger {
	return log.NewNOPFactory().Logger()
}

func newTestCCMExternalCredential(t *testing.T, body string, headers http.Header) (*externalCredential, observable.Subscription[struct{}]) {
	t.Helper()
	subscriber := observable.NewSubscriber[struct{}](8)
	subscription, _ := subscriber.Subscription()
	credential := &externalCredential{
		tag:          "test",
		baseURL:      "http://example.com",
		token:        "token",
		pollInterval: 25 * time.Millisecond,
		forwardHTTPClient: &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.String() != "http://example.com/ccm/v1/status?watch=true" {
				t.Fatalf("unexpected request URL: %s", request.URL.String())
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     headers.Clone(),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		logger:           newTestLogger(),
		statusSubscriber: subscriber,
	}
	return credential, subscription
}

func newTestYamuxSessionPair(t *testing.T) (*yamux.Session, *yamux.Session) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	clientSession, err := yamux.Client(clientConn, defaultYamuxConfig)
	if err != nil {
		t.Fatalf("create yamux client: %v", err)
	}
	serverSession, err := yamux.Server(serverConn, defaultYamuxConfig)
	if err != nil {
		clientSession.Close()
		t.Fatalf("create yamux server: %v", err)
	}
	t.Cleanup(func() {
		clientSession.Close()
		serverSession.Close()
	})
	return clientSession, serverSession
}

func TestExternalCredentialConnectStatusStreamOneShotRestoresLastUpdated(t *testing.T) {
	credential, subscription := newTestCCMExternalCredential(t, "{\"five_hour_utilization\":12,\"weekly_utilization\":34,\"plan_weight\":2}\n", nil)
	oldTime := time.Unix(123, 0)
	credential.stateAccess.Lock()
	credential.state.lastUpdated = oldTime
	credential.stateAccess.Unlock()

	result, err := credential.connectStatusStream(context.Background())
	if err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
	if !result.oneShot {
		t.Fatal("expected one-shot result")
	}
	if result.frames != 1 {
		t.Fatalf("expected 1 frame, got %d", result.frames)
	}
	if !credential.lastUpdatedTime().Equal(oldTime) {
		t.Fatalf("expected lastUpdated restored to %v, got %v", oldTime, credential.lastUpdatedTime())
	}
	if credential.fiveHourUtilization() != 12 || credential.weeklyUtilization() != 34 {
		t.Fatalf("unexpected utilizations: 5h=%v weekly=%v", credential.fiveHourUtilization(), credential.weeklyUtilization())
	}
	if count := drainStatusEvents(subscription); count != 1 {
		t.Fatalf("expected 1 status event, got %d", count)
	}

	failures, backoff, oneShot := credential.nextStatusStreamBackoff(result, 3)
	if !oneShot {
		t.Fatal("expected one-shot backoff branch")
	}
	if failures != 0 {
		t.Fatalf("expected failures reset, got %d", failures)
	}
	if backoff != credential.pollInterval {
		t.Fatalf("expected poll interval backoff %v, got %v", credential.pollInterval, backoff)
	}
}

func TestExternalCredentialConnectStatusStreamSingleFrameStreamReconnects(t *testing.T) {
	headers := make(http.Header)
	headers.Set(statusStreamHeader, "true")
	credential, subscription := newTestCCMExternalCredential(t, "{\"five_hour_utilization\":12,\"weekly_utilization\":34,\"plan_weight\":2}\n", headers)
	oldTime := time.Unix(123, 0)
	credential.stateAccess.Lock()
	credential.state.lastUpdated = oldTime
	credential.stateAccess.Unlock()

	result, err := credential.connectStatusStream(context.Background())
	if err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
	if result.oneShot {
		t.Fatal("did not expect one-shot result")
	}
	if result.frames != 1 {
		t.Fatalf("expected 1 frame, got %d", result.frames)
	}
	if credential.lastUpdatedTime().Equal(oldTime) {
		t.Fatal("expected lastUpdated to remain refreshed")
	}
	if credential.fiveHourUtilization() != 12 || credential.weeklyUtilization() != 34 {
		t.Fatalf("unexpected utilizations: 5h=%v weekly=%v", credential.fiveHourUtilization(), credential.weeklyUtilization())
	}
	if count := drainStatusEvents(subscription); count != 1 {
		t.Fatalf("expected 1 status event, got %d", count)
	}

	failures, backoff, oneShot := credential.nextStatusStreamBackoff(result, 3)
	if oneShot {
		t.Fatal("did not expect one-shot backoff branch")
	}
	if failures != 4 {
		t.Fatalf("expected failures incremented to 4, got %d", failures)
	}
	if backoff < 16*time.Second || backoff >= 24*time.Second {
		t.Fatalf("expected connector backoff in [16s, 24s), got %v", backoff)
	}
}

func TestExternalCredentialConnectStatusStreamMultiFrameKeepsLastUpdated(t *testing.T) {
	credential, subscription := newTestCCMExternalCredential(t, strings.Join([]string{
		"{\"five_hour_utilization\":12,\"weekly_utilization\":34,\"plan_weight\":2}",
		"{\"five_hour_utilization\":13,\"weekly_utilization\":35,\"plan_weight\":3}",
	}, "\n"), nil)
	oldTime := time.Unix(123, 0)
	credential.stateAccess.Lock()
	credential.state.lastUpdated = oldTime
	credential.stateAccess.Unlock()

	result, err := credential.connectStatusStream(context.Background())
	if err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
	if result.oneShot {
		t.Fatal("did not expect one-shot result")
	}
	if result.frames != 2 {
		t.Fatalf("expected 2 frames, got %d", result.frames)
	}
	if credential.lastUpdatedTime().Equal(oldTime) {
		t.Fatal("expected lastUpdated to remain refreshed")
	}
	if credential.fiveHourUtilization() != 13 || credential.weeklyUtilization() != 35 {
		t.Fatalf("unexpected utilizations: 5h=%v weekly=%v", credential.fiveHourUtilization(), credential.weeklyUtilization())
	}
	if count := drainStatusEvents(subscription); count != 2 {
		t.Fatalf("expected 2 status events, got %d", count)
	}
}

func TestDefaultCredentialStatusChangesEmitStatus(t *testing.T) {
	credentialPath := filepath.Join(t.TempDir(), "credentials.json")
	err := os.WriteFile(credentialPath, []byte("{\"claudeAiOauth\":{\"accessToken\":\"token\",\"refreshToken\":\"\",\"expiresAt\":0,\"subscriptionType\":\"max\"}}\n"), 0o600)
	if err != nil {
		t.Fatalf("write credential file: %v", err)
	}

	subscriber := observable.NewSubscriber[struct{}](8)
	subscription, _ := subscriber.Subscription()
	credential := &defaultCredential{
		tag:              "test",
		credentialPath:   credentialPath,
		logger:           newTestLogger(),
		statusSubscriber: subscriber,
	}

	err = credential.markCredentialsUnavailable(errors.New("boom"))
	if err == nil {
		t.Fatal("expected error from markCredentialsUnavailable")
	}
	if count := drainStatusEvents(subscription); count != 1 {
		t.Fatalf("expected 1 status event after unavailable transition, got %d", count)
	}

	err = credential.reloadCredentials(true)
	if err != nil {
		t.Fatalf("reload credentials: %v", err)
	}
	if count := drainStatusEvents(subscription); count != 1 {
		t.Fatalf("expected 1 status event after recovery, got %d", count)
	}
	if weight := credential.planWeight(); weight != 5 {
		t.Fatalf("expected initial max weight 5, got %v", weight)
	}

	profileClient := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				"{\"organization\":{\"organization_type\":\"claude_max\",\"rate_limit_tier\":\"default_claude_max_20x\"}}",
			)),
		}, nil
	})}
	credential.fetchProfile(context.Background(), profileClient, "token")
	if count := drainStatusEvents(subscription); count != 1 {
		t.Fatalf("expected 1 status event after weight change, got %d", count)
	}
	if weight := credential.planWeight(); weight != 10 {
		t.Fatalf("expected upgraded max weight 10, got %v", weight)
	}
}

func TestExternalCredentialReverseSessionChangesEmitStatus(t *testing.T) {
	subscriber := observable.NewSubscriber[struct{}](8)
	subscription, _ := subscriber.Subscription()
	credential := &externalCredential{
		tag:              "receiver",
		baseURL:          reverseProxyBaseURL,
		pollInterval:     time.Minute,
		logger:           newTestLogger(),
		statusSubscriber: subscriber,
	}

	clientSession, _ := newTestYamuxSessionPair(t)
	if !credential.setReverseSession(clientSession) {
		t.Fatal("expected reverse session to be accepted")
	}
	if count := drainStatusEvents(subscription); count != 1 {
		t.Fatalf("expected 1 status event after reverse session up, got %d", count)
	}
	if !credential.isAvailable() {
		t.Fatal("expected receiver credential to become available")
	}

	credential.clearReverseSession(clientSession)
	if count := drainStatusEvents(subscription); count != 1 {
		t.Fatalf("expected 1 status event after reverse session down, got %d", count)
	}
	if credential.isAvailable() {
		t.Fatal("expected receiver credential to become unavailable")
	}
}
