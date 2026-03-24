package ccm

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newHandlerCredential(t *testing.T, transport http.RoundTripper) (*defaultCredential, string) {
	t.Helper()
	directory := t.TempDir()
	credentialPath := filepath.Join(directory, ".credentials.json")
	writeTestCredentials(t, credentialPath, &oauthCredentials{
		AccessToken:      "old-token",
		RefreshToken:     "refresh-token",
		ExpiresAt:        time.Now().Add(time.Hour).UnixMilli(),
		Scopes:           []string{"user:profile", "user:inference"},
		SubscriptionType: optionalStringPointer("max"),
		RateLimitTier:    optionalStringPointer("default_claude_max_20x"),
	})
	credential := newTestDefaultCredential(t, credentialPath, transport)
	if err := credential.reloadCredentials(true); err != nil {
		t.Fatal(err)
	}
	seedTestCredentialState(credential)
	return credential, credentialPath
}

func TestServiceHandlerRecoversFrom401(t *testing.T) {
	t.Parallel()

	var messageRequests atomic.Int32
	var refreshRequests atomic.Int32
	credential, _ := newHandlerCredential(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/v1/messages":
			call := messageRequests.Add(1)
			switch request.Header.Get("Authorization") {
			case "Bearer old-token":
				if call != 1 {
					t.Fatalf("unexpected old-token call count %d", call)
				}
				return newTextResponse(http.StatusUnauthorized, "unauthorized"), nil
			case "Bearer new-token":
				return newJSONResponse(http.StatusOK, `{}`), nil
			default:
				t.Fatalf("unexpected authorization header %q", request.Header.Get("Authorization"))
			}
		case "/v1/oauth/token":
			refreshRequests.Add(1)
			return newJSONResponse(http.StatusOK, `{"access_token":"new-token","refresh_token":"new-refresh","expires_in":3600}`), nil
		default:
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
		return nil, nil
	}))

	service := newTestService(credential)
	recorder := httptest.NewRecorder()
	service.ServeHTTP(recorder, newMessageRequest(`{"model":"claude","messages":[],"metadata":{"user_id":"{\"session_id\":\"session\"}"}}`))

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if messageRequests.Load() != 2 {
		t.Fatalf("expected two upstream message requests, got %d", messageRequests.Load())
	}
	if refreshRequests.Load() != 1 {
		t.Fatalf("expected one refresh request, got %d", refreshRequests.Load())
	}
}

func TestServiceHandlerRecoversFromRevoked403(t *testing.T) {
	t.Parallel()

	var messageRequests atomic.Int32
	var refreshRequests atomic.Int32
	credential, _ := newHandlerCredential(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/v1/messages":
			messageRequests.Add(1)
			if request.Header.Get("Authorization") == "Bearer old-token" {
				return newTextResponse(http.StatusForbidden, "OAuth token has been revoked"), nil
			}
			return newJSONResponse(http.StatusOK, `{}`), nil
		case "/v1/oauth/token":
			refreshRequests.Add(1)
			return newJSONResponse(http.StatusOK, `{"access_token":"new-token","refresh_token":"new-refresh","expires_in":3600}`), nil
		default:
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
		return nil, nil
	}))

	service := newTestService(credential)
	recorder := httptest.NewRecorder()
	service.ServeHTTP(recorder, newMessageRequest(`{"model":"claude","messages":[],"metadata":{"user_id":"{\"session_id\":\"session\"}"}}`))

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if refreshRequests.Load() != 1 {
		t.Fatalf("expected one refresh request, got %d", refreshRequests.Load())
	}
}

func TestServiceHandlerDoesNotRecoverFromOrdinary403(t *testing.T) {
	t.Parallel()

	var refreshRequests atomic.Int32
	credential, _ := newHandlerCredential(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/v1/messages":
			return newTextResponse(http.StatusForbidden, "forbidden"), nil
		case "/v1/oauth/token":
			refreshRequests.Add(1)
			return newJSONResponse(http.StatusOK, `{"access_token":"new-token","refresh_token":"new-refresh","expires_in":3600}`), nil
		default:
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
		return nil, nil
	}))

	service := newTestService(credential)
	recorder := httptest.NewRecorder()
	service.ServeHTTP(recorder, newMessageRequest(`{"model":"claude","messages":[],"metadata":{"user_id":"{\"session_id\":\"session\"}"}}`))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", recorder.Code)
	}
	if refreshRequests.Load() != 0 {
		t.Fatalf("expected no refresh request, got %d", refreshRequests.Load())
	}
	if !strings.Contains(recorder.Body.String(), "forbidden") {
		t.Fatalf("expected forbidden body, got %s", recorder.Body.String())
	}
}

func TestServiceHandlerUsesReloadedTokenBeforeRefreshing(t *testing.T) {
	t.Parallel()

	var messageRequests atomic.Int32
	var refreshRequests atomic.Int32
	var credentialPath string
	var credential *defaultCredential
	credential, credentialPath = newHandlerCredential(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/v1/messages":
			call := messageRequests.Add(1)
			if request.Header.Get("Authorization") == "Bearer old-token" {
				updatedCredentials := readTestCredentials(t, credentialPath)
				updatedCredentials.AccessToken = "disk-token"
				updatedCredentials.ExpiresAt = time.Now().Add(time.Hour).UnixMilli()
				writeTestCredentials(t, credentialPath, updatedCredentials)
				if call != 1 {
					t.Fatalf("unexpected old-token call count %d", call)
				}
				return newTextResponse(http.StatusUnauthorized, "unauthorized"), nil
			}
			if request.Header.Get("Authorization") != "Bearer disk-token" {
				t.Fatalf("expected disk token retry, got %q", request.Header.Get("Authorization"))
			}
			return newJSONResponse(http.StatusOK, `{}`), nil
		case "/v1/oauth/token":
			refreshRequests.Add(1)
			return newJSONResponse(http.StatusOK, `{"access_token":"new-token","refresh_token":"new-refresh","expires_in":3600}`), nil
		default:
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
		return nil, nil
	}))

	service := newTestService(credential)
	recorder := httptest.NewRecorder()
	service.ServeHTTP(recorder, newMessageRequest(`{"model":"claude","messages":[],"metadata":{"user_id":"{\"session_id\":\"session\"}"}}`))

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if refreshRequests.Load() != 0 {
		t.Fatalf("expected zero refresh requests, got %d", refreshRequests.Load())
	}
}

func TestServiceHandlerRetriesAuthRecoveryOnlyOnce(t *testing.T) {
	t.Parallel()

	var messageRequests atomic.Int32
	var refreshRequests atomic.Int32
	credential, _ := newHandlerCredential(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/v1/messages":
			messageRequests.Add(1)
			return newTextResponse(http.StatusUnauthorized, "still unauthorized"), nil
		case "/v1/oauth/token":
			refreshRequests.Add(1)
			return newJSONResponse(http.StatusOK, `{"access_token":"new-token","refresh_token":"new-refresh","expires_in":3600}`), nil
		default:
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
		return nil, nil
	}))

	service := newTestService(credential)
	recorder := httptest.NewRecorder()
	service.ServeHTTP(recorder, newMessageRequest(`{"model":"claude","messages":[],"metadata":{"user_id":"{\"session_id\":\"session\"}"}}`))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", recorder.Code)
	}
	if messageRequests.Load() != 2 {
		t.Fatalf("expected exactly two upstream attempts, got %d", messageRequests.Load())
	}
	if refreshRequests.Load() != 1 {
		t.Fatalf("expected exactly one refresh request, got %d", refreshRequests.Load())
	}
}
