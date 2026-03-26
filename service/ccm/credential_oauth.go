package ccm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sagernet/sing-box/log"
	E "github.com/sagernet/sing/common/exceptions"
)

const (
	oauth2ClientID          = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	oauth2TokenURL          = "https://platform.claude.com/v1/oauth/token"
	claudeAPIBaseURL        = "https://api.anthropic.com"
	anthropicBetaOAuthValue = "oauth-2025-04-20"

	// ref (@anthropic-ai/claude-code @2.1.81): cli.js vB (line 172879)
	tokenRefreshBufferMs = 300000
)

// ref (@anthropic-ai/claude-code @2.1.81): cli.js q78 (line 33167)
// These scopes may change across Claude Code versions.
var defaultOAuthScopes = []string{
	"user:profile", "user:inference", "user:sessions:claude_code",
	"user:mcp_servers", "user:file_upload",
}

// resolveRefreshScopes determines which scopes to send in the token refresh request.
//
// ref (@anthropic-ai/claude-code @2.1.81): cli.js NR() (line 172693) + mB6 scope logic (line 172761)
//
// Claude Code behavior: if stored scopes include "user:inference", send default
// scopes; otherwise send the stored scopes verbatim.
func resolveRefreshScopes(stored []string) string {
	if len(stored) == 0 || slices.Contains(stored, "user:inference") {
		return strings.Join(defaultOAuthScopes, " ")
	}
	return strings.Join(stored, " ")
}

const (
	ccmRefreshUserAgent  = "axios/1.13.6"
	ccmUserAgentFallback = "claude-code/2.1.85"
)

var (
	ccmUserAgentOnce  sync.Once
	ccmUserAgentValue string
)

func initCCMUserAgent(logger log.ContextLogger) {
	ccmUserAgentOnce.Do(func() {
		version, err := detectClaudeCodeVersion()
		if err != nil {
			logger.Error("detect Claude Code version: ", err)
			ccmUserAgentValue = ccmUserAgentFallback
			return
		}
		logger.Debug("detected Claude Code version: ", version)
		ccmUserAgentValue = "claude-code/" + version
	})
}

func detectClaudeCodeVersion() (string, error) {
	userInfo, err := getRealUser()
	if err != nil {
		return "", E.Cause(err, "get user")
	}
	binaryName := "claude"
	if runtime.GOOS == "windows" {
		binaryName = "claude.exe"
	}
	linkPath := filepath.Join(userInfo.HomeDir, ".local", "bin", binaryName)
	target, err := os.Readlink(linkPath)
	if err != nil {
		return "", E.Cause(err, "readlink ", linkPath)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(linkPath), target)
	}
	parent := filepath.Base(filepath.Dir(target))
	if parent != "versions" {
		return "", E.New("unexpected symlink target: ", target)
	}
	return filepath.Base(target), nil
}

// resolveConfigDir returns the Claude config directory for lock coordination.
//
// ref (@anthropic-ai/claude-code @2.1.81): cli.js d1() (line 2983) — config dir used for locking
func resolveConfigDir(credentialPath string, credentialFilePath string) string {
	if credentialPath == "" {
		if configDir := os.Getenv("CLAUDE_CONFIG_DIR"); configDir != "" {
			return configDir
		}
		userInfo, err := getRealUser()
		if err == nil {
			return filepath.Join(userInfo.HomeDir, ".claude")
		}
	}
	return filepath.Dir(credentialFilePath)
}

func getRealUser() (*user.User, error) {
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		sudoUserInfo, err := user.Lookup(sudoUser)
		if err == nil {
			return sudoUserInfo, nil
		}
	}
	return user.Current()
}

func getDefaultCredentialsPath() (string, error) {
	if configDir := os.Getenv("CLAUDE_CONFIG_DIR"); configDir != "" {
		return filepath.Join(configDir, ".credentials.json"), nil
	}
	userInfo, err := getRealUser()
	if err != nil {
		return "", err
	}
	return filepath.Join(userInfo.HomeDir, ".claude", ".credentials.json"), nil
}

func readCredentialsFromFile(path string) (*oauthCredentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var credentialsContainer struct {
		ClaudeAIAuth *oauthCredentials `json:"claudeAiOauth,omitempty"`
	}
	err = json.Unmarshal(data, &credentialsContainer)
	if err != nil {
		return nil, err
	}
	if credentialsContainer.ClaudeAIAuth == nil {
		return nil, E.New("claudeAiOauth field not found in credentials")
	}
	return credentialsContainer.ClaudeAIAuth, nil
}

func checkCredentialFileWritable(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	return file.Close()
}

// writeCredentialsToFile performs a read-modify-write: reads the existing JSON,
// replaces only the claudeAiOauth key, and writes back. This preserves any
// other top-level keys in the credential file.
//
// ref (@anthropic-ai/claude-code @2.1.81): cli.js BP6 (line 179444-179454) — read-modify-write
// ref: cli.js qD1.update (line 176156) — writeFileSync + chmod 0o600
func writeCredentialsToFile(credentials *oauthCredentials, path string) error {
	return writeStorageValue(jsonFileStorage{path: path}, "claudeAiOauth", credentials)
}

// oauthCredentials mirrors the claudeAiOauth object in Claude Code's
// credential file ($CLAUDE_CONFIG_DIR/.credentials.json).
//
// ref (@anthropic-ai/claude-code @2.1.81): cli.js BP6 (line 179446-179452)
type oauthCredentials struct {
	AccessToken      string   `json:"accessToken"`      // ref: cli.js line 179447
	RefreshToken     string   `json:"refreshToken"`     // ref: cli.js line 179448
	ExpiresAt        int64    `json:"expiresAt"`        // ref: cli.js line 179449 (epoch ms)
	Scopes           []string `json:"scopes"`           // ref: cli.js line 179450
	SubscriptionType *string  `json:"subscriptionType"` // ref: cli.js line 179451 (?? null)
	RateLimitTier    *string  `json:"rateLimitTier"`    // ref: cli.js line 179452 (?? null)
}

type oauthRefreshResult struct {
	Credentials  *oauthCredentials
	TokenAccount *claudeOAuthAccount
	Profile      *claudeProfileSnapshot
}

func (c *oauthCredentials) needsRefresh() bool {
	if c.ExpiresAt == 0 {
		return false
	}
	return time.Now().UnixMilli() >= c.ExpiresAt-tokenRefreshBufferMs
}

func refreshToken(ctx context.Context, httpClient *http.Client, credentials *oauthCredentials) (*oauthRefreshResult, time.Duration, error) {
	if credentials.RefreshToken == "" {
		return nil, 0, E.New("refresh token is empty")
	}

	// ref (@anthropic-ai/claude-code @2.1.81): cli.js mB6 (line 172757-172761)
	requestBody, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": credentials.RefreshToken,
		"client_id":     oauth2ClientID,
		"scope":         resolveRefreshScopes(credentials.Scopes),
	})
	if err != nil {
		return nil, 0, E.Cause(err, "marshal request")
	}

	response, err := doHTTPWithRetry(ctx, httpClient, func() (*http.Request, error) {
		request, err := http.NewRequest("POST", oauth2TokenURL, bytes.NewReader(requestBody))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("User-Agent", ccmRefreshUserAgent)
		return request, nil
	})
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusTooManyRequests {
		body, _ := io.ReadAll(response.Body)
		retryDelay := time.Duration(-1)
		if retryAfter := response.Header.Get("Retry-After"); retryAfter != "" {
			seconds, parseErr := strconv.ParseInt(retryAfter, 10, 64)
			if parseErr == nil && seconds > 0 {
				retryDelay = time.Duration(seconds) * time.Second
			}
		}
		return nil, retryDelay, E.New("refresh rate limited: ", response.Status, " ", string(body))
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		return nil, 0, E.New("refresh failed: ", response.Status, " ", string(body))
	}

	// ref (@anthropic-ai/claude-code @2.1.81): cli.js mB6 response (line 172769-172772)
	var tokenResponse struct {
		AccessToken  string  `json:"access_token"`  // ref: cli.js line 172770 z
		RefreshToken string  `json:"refresh_token"` // ref: cli.js line 172770 w (defaults to input)
		ExpiresIn    int     `json:"expires_in"`    // ref: cli.js line 172770 O
		Scope        *string `json:"scope"`         // ref: cli.js line 172772 uB6(Y.scope)
		Account      *struct {
			UUID         string `json:"uuid"`
			EmailAddress string `json:"email_address"`
		} `json:"account"`
		Organization *struct {
			UUID string `json:"uuid"`
		} `json:"organization"`
	}
	err = json.NewDecoder(response.Body).Decode(&tokenResponse)
	if err != nil {
		return nil, 0, E.Cause(err, "decode response")
	}

	newCredentials := *credentials
	newCredentials.AccessToken = tokenResponse.AccessToken
	if tokenResponse.RefreshToken != "" {
		newCredentials.RefreshToken = tokenResponse.RefreshToken
	}
	newCredentials.ExpiresAt = time.Now().UnixMilli() + int64(tokenResponse.ExpiresIn)*1000
	// ref: cli.js uB6 (line 172696-172697): A?.split(" ").filter(Boolean)
	// strings.Fields matches .filter(Boolean): splits on whitespace runs, removes empty strings
	if tokenResponse.Scope != nil {
		newCredentials.Scopes = strings.Fields(*tokenResponse.Scope)
	}

	return &oauthRefreshResult{
		Credentials:  &newCredentials,
		TokenAccount: extractTokenAccount(tokenResponse.Account, tokenResponse.Organization),
	}, 0, nil
}

func cloneCredentials(credentials *oauthCredentials) *oauthCredentials {
	if credentials == nil {
		return nil
	}
	cloned := *credentials
	cloned.Scopes = append([]string(nil), credentials.Scopes...)
	cloned.SubscriptionType = cloneStringPointer(credentials.SubscriptionType)
	cloned.RateLimitTier = cloneStringPointer(credentials.RateLimitTier)
	return &cloned
}

func credentialsEqual(left *oauthCredentials, right *oauthCredentials) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.AccessToken == right.AccessToken &&
		left.RefreshToken == right.RefreshToken &&
		left.ExpiresAt == right.ExpiresAt &&
		slices.Equal(left.Scopes, right.Scopes) &&
		equalStringPointer(left.SubscriptionType, right.SubscriptionType) &&
		equalStringPointer(left.RateLimitTier, right.RateLimitTier)
}

func extractTokenAccount(account *struct {
	UUID         string `json:"uuid"`
	EmailAddress string `json:"email_address"`
}, organization *struct {
	UUID string `json:"uuid"`
},
) *claudeOAuthAccount {
	if account == nil && organization == nil {
		return nil
	}
	tokenAccount := &claudeOAuthAccount{}
	if account != nil {
		tokenAccount.AccountUUID = account.UUID
		tokenAccount.EmailAddress = account.EmailAddress
	}
	if organization != nil {
		tokenAccount.OrganizationUUID = organization.UUID
	}
	if tokenAccount.AccountUUID == "" && tokenAccount.EmailAddress == "" && tokenAccount.OrganizationUUID == "" {
		return nil
	}
	return tokenAccount
}
