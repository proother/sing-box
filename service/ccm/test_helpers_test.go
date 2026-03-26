package ccm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func newJSONResponse(statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Status:     http.StatusText(statusCode),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func newTextResponse(statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Status:     http.StatusText(statusCode),
		Header:     http.Header{"Content-Type": []string{"text/plain"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func writeTestCredentials(t *testing.T, path string, credentials *oauthCredentials) {
	t.Helper()
	if path == "" {
		var err error
		path, err = getDefaultCredentialsPath()
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := writeCredentialsToFile(credentials, path); err != nil {
		t.Fatal(err)
	}
}

func readTestCredentials(t *testing.T, path string) *oauthCredentials {
	t.Helper()
	if path == "" {
		var err error
		path, err = getDefaultCredentialsPath()
		if err != nil {
			t.Fatal(err)
		}
	}
	credentials, err := readCredentialsFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return credentials
}

func newTestDefaultCredential(t *testing.T, credentialPath string, transport http.RoundTripper) *defaultCredential {
	t.Helper()
	credentialFilePath, err := resolveCredentialFilePath(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	requestContext, cancelRequests := context.WithCancel(context.Background())
	credential := &defaultCredential{
		tag:                "test",
		serviceContext:     context.Background(),
		credentialPath:     credentialPath,
		credentialFilePath: credentialFilePath,
		configDir:          resolveConfigDir(credentialPath, credentialFilePath),
		syncClaudeConfig:   credentialPath == "",
		cap5h:              99,
		capWeekly:          99,
		forwardHTTPClient:  &http.Client{Transport: transport},
		acquireLock:        acquireCredentialLock,
		logger:             log.NewNOPFactory().Logger(),
		requestContext:     requestContext,
		cancelRequests:     cancelRequests,
	}
	if credential.syncClaudeConfig {
		credential.claudeDirectory = credential.configDir
		credential.claudeConfigPath = resolveClaudeConfigWritePath(credential.claudeDirectory)
	}
	credential.state.lastUpdated = time.Now()
	return credential
}

func seedTestCredentialState(credential *defaultCredential) {
	billingType := "individual"
	accountCreatedAt := "2024-01-01T00:00:00Z"
	subscriptionCreatedAt := "2024-01-02T00:00:00Z"
	credential.stateAccess.Lock()
	credential.state.accountUUID = "account"
	credential.state.accountType = "max"
	credential.state.rateLimitTier = "default_claude_max_20x"
	credential.state.oauthAccount = &claudeOAuthAccount{
		AccountUUID:           "account",
		EmailAddress:          "user@example.com",
		OrganizationUUID:      "org",
		BillingType:           &billingType,
		AccountCreatedAt:      &accountCreatedAt,
		SubscriptionCreatedAt: &subscriptionCreatedAt,
	}
	credential.stateAccess.Unlock()
}

func newTestService(credential *defaultCredential) *Service {
	return &Service{
		logger:        log.NewNOPFactory().Logger(),
		options:       option.CCMServiceOptions{Credentials: []option.CCMCredential{{Tag: "default"}}},
		httpHeaders:   make(http.Header),
		providers:     map[string]credentialProvider{"default": &singleCredentialProvider{credential: credential}},
		sessionModels: make(map[sessionModelKey]time.Time),
	}
}

func newMessageRequest(body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func tempConfigPath(t *testing.T, dir string) string {
	t.Helper()
	return filepath.Join(dir, claudeCodeLegacyConfigFileName())
}
