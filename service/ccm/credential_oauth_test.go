package ccm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRefreshTokenScopeParsing(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		storedScopes  []string
		responseBody  string
		expectedScope string
		expected      []string
	}{
		{
			name:          "missing scope preserves stored scopes",
			storedScopes:  []string{"user:profile", "user:inference"},
			responseBody:  `{"access_token":"new-token","refresh_token":"new-refresh","expires_in":3600}`,
			expectedScope: strings.Join(defaultOAuthScopes, " "),
			expected:      []string{"user:profile", "user:inference"},
		},
		{
			name:          "empty scope clears stored scopes",
			storedScopes:  []string{"user:profile", "user:inference"},
			responseBody:  `{"access_token":"new-token","refresh_token":"new-refresh","expires_in":3600,"scope":""}`,
			expectedScope: strings.Join(defaultOAuthScopes, " "),
			expected:      []string{},
		},
		{
			name:          "stored non inference scopes are sent verbatim",
			storedScopes:  []string{"user:profile"},
			responseBody:  `{"access_token":"new-token","refresh_token":"new-refresh","expires_in":3600,"scope":"user:profile user:file_upload"}`,
			expectedScope: "user:profile",
			expected:      []string{"user:profile", "user:file_upload"},
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			var seenScope string
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Fatal(err)
				}
				var payload map[string]string
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatal(err)
				}
				seenScope = payload["scope"]
				return newJSONResponse(http.StatusOK, testCase.responseBody), nil
			})}

			result, _, err := refreshToken(context.Background(), client, &oauthCredentials{
				AccessToken:  "old-token",
				RefreshToken: "refresh-token",
				ExpiresAt:    time.Now().Add(-time.Minute).UnixMilli(),
				Scopes:       testCase.storedScopes,
			})
			if err != nil {
				t.Fatal(err)
			}
			if seenScope != testCase.expectedScope {
				t.Fatalf("expected request scope %q, got %q", testCase.expectedScope, seenScope)
			}
			if result == nil || result.Credentials == nil {
				t.Fatal("expected refresh result credentials")
			}
			if !slices.Equal(result.Credentials.Scopes, testCase.expected) {
				t.Fatalf("expected scopes %v, got %v", testCase.expected, result.Credentials.Scopes)
			}
		})
	}
}

func TestRefreshTokenExtractsTokenAccount(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return newJSONResponse(http.StatusOK, `{
			"access_token":"new-token",
			"refresh_token":"new-refresh",
			"expires_in":3600,
			"account":{"uuid":"account","email_address":"user@example.com"},
			"organization":{"uuid":"org"}
		}`), nil
	})}

	result, _, err := refreshToken(context.Background(), client, &oauthCredentials{
		AccessToken:  "old-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(-time.Minute).UnixMilli(),
		Scopes:       []string{"user:profile", "user:inference"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.TokenAccount == nil {
		t.Fatal("expected token account")
	}
	if result.TokenAccount.AccountUUID != "account" || result.TokenAccount.EmailAddress != "user@example.com" || result.TokenAccount.OrganizationUUID != "org" {
		t.Fatalf("unexpected token account: %#v", result.TokenAccount)
	}
}

func TestCredentialsEqualIncludesProfileFields(t *testing.T) {
	t.Parallel()

	subscriptionType := "max"
	rateLimitTier := "default_claude_max_20x"
	left := &oauthCredentials{
		AccessToken:      "token",
		RefreshToken:     "refresh",
		ExpiresAt:        123,
		Scopes:           []string{"user:inference"},
		SubscriptionType: &subscriptionType,
		RateLimitTier:    &rateLimitTier,
	}
	right := cloneCredentials(left)
	if !credentialsEqual(left, right) {
		t.Fatal("expected cloned credentials to be equal")
	}

	otherTier := "default_claude_max_5x"
	right.RateLimitTier = &otherTier
	if credentialsEqual(left, right) {
		t.Fatal("expected different rate limit tier to break equality")
	}
}
