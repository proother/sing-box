package ccm

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// claudeCodeConfig represents the persisted config written by Claude Code.
//
// ref (@anthropic-ai/claude-code @2.1.81):
//
//	ref: cli.js P8() (line 174997) — reads config
//	ref: cli.js c8() (line 174919) — writes config
//	ref: cli.js _D() (line 39158-39163) — config file path resolution
type claudeCodeConfig struct {
	UserID       string              `json:"userID"`       // ref: cli.js XL() (line 175325) — random 32-byte hex, generated once
	OAuthAccount *claudeOAuthAccount `json:"oauthAccount"` // ref: cli.js fP6() / storeOAuthAccountInfo — from /api/oauth/profile
}

type claudeOAuthAccount struct {
	AccountUUID           string  `json:"accountUuid,omitempty"`
	EmailAddress          string  `json:"emailAddress,omitempty"`
	OrganizationUUID      string  `json:"organizationUuid,omitempty"`
	DisplayName           *string `json:"displayName,omitempty"`
	HasExtraUsageEnabled  *bool   `json:"hasExtraUsageEnabled,omitempty"`
	BillingType           *string `json:"billingType,omitempty"`
	AccountCreatedAt      *string `json:"accountCreatedAt,omitempty"`
	SubscriptionCreatedAt *string `json:"subscriptionCreatedAt,omitempty"`
}

// resolveClaudeConfigFile finds the Claude Code config file within the given directory.
//
// Config file path resolution mirrors cli.js _D() (line 39158-39163):
//  1. claudeDirectory/.config.json — newer format, checked first
//  2. claudeDirectory/.claude.json — used when CLAUDE_CONFIG_DIR is set
//  3. filepath.Dir(claudeDirectory)/.claude.json — default ~/.claude case → ~/.claude.json
//
// Returns the first path that exists, or "" if none found.
func resolveClaudeConfigFile(claudeDirectory string) string {
	candidates := []string{
		filepath.Join(claudeDirectory, ".config.json"),
		filepath.Join(claudeDirectory, claudeCodeLegacyConfigFileName()),
		filepath.Join(filepath.Dir(claudeDirectory), claudeCodeLegacyConfigFileName()),
	}
	for _, candidate := range candidates {
		_, err := os.Stat(candidate)
		if err == nil {
			return candidate
		}
	}
	return ""
}

func readClaudeCodeConfig(path string) (*claudeCodeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var config claudeCodeConfig
	err = json.Unmarshal(data, &config)
	if err != nil {
		return nil, err
	}
	return &config, nil
}

func resolveClaudeConfigWritePath(claudeDirectory string) string {
	if claudeDirectory == "" {
		return ""
	}
	existingPath := resolveClaudeConfigFile(claudeDirectory)
	if existingPath != "" {
		return existingPath
	}
	if os.Getenv("CLAUDE_CONFIG_DIR") != "" {
		return filepath.Join(claudeDirectory, claudeCodeLegacyConfigFileName())
	}
	defaultClaudeDirectory := filepath.Join(filepath.Dir(claudeDirectory), ".claude")
	if claudeDirectory != defaultClaudeDirectory {
		return filepath.Join(claudeDirectory, claudeCodeLegacyConfigFileName())
	}
	return filepath.Join(filepath.Dir(claudeDirectory), claudeCodeLegacyConfigFileName())
}

func writeClaudeCodeOAuthAccount(path string, account *claudeOAuthAccount) error {
	if path == "" || account == nil {
		return nil
	}
	storage := jsonFileStorage{path: path}
	return writeStorageValue(storage, "oauthAccount", account)
}

func claudeCodeLegacyConfigFileName() string {
	if os.Getenv("CLAUDE_CODE_CUSTOM_OAUTH_URL") != "" {
		return ".claude-custom-oauth.json"
	}
	return ".claude.json"
}

func cloneClaudeOAuthAccount(account *claudeOAuthAccount) *claudeOAuthAccount {
	if account == nil {
		return nil
	}
	cloned := *account
	cloned.DisplayName = cloneStringPointer(account.DisplayName)
	cloned.HasExtraUsageEnabled = cloneBoolPointer(account.HasExtraUsageEnabled)
	cloned.BillingType = cloneStringPointer(account.BillingType)
	cloned.AccountCreatedAt = cloneStringPointer(account.AccountCreatedAt)
	cloned.SubscriptionCreatedAt = cloneStringPointer(account.SubscriptionCreatedAt)
	return &cloned
}

func mergeClaudeOAuthAccount(base *claudeOAuthAccount, update *claudeOAuthAccount) *claudeOAuthAccount {
	if update == nil {
		return cloneClaudeOAuthAccount(base)
	}
	if base == nil {
		return cloneClaudeOAuthAccount(update)
	}
	merged := cloneClaudeOAuthAccount(base)
	if update.AccountUUID != "" {
		merged.AccountUUID = update.AccountUUID
	}
	if update.EmailAddress != "" {
		merged.EmailAddress = update.EmailAddress
	}
	if update.OrganizationUUID != "" {
		merged.OrganizationUUID = update.OrganizationUUID
	}
	if update.DisplayName != nil {
		merged.DisplayName = cloneStringPointer(update.DisplayName)
	}
	if update.HasExtraUsageEnabled != nil {
		merged.HasExtraUsageEnabled = cloneBoolPointer(update.HasExtraUsageEnabled)
	}
	if update.BillingType != nil {
		merged.BillingType = cloneStringPointer(update.BillingType)
	}
	if update.AccountCreatedAt != nil {
		merged.AccountCreatedAt = cloneStringPointer(update.AccountCreatedAt)
	}
	if update.SubscriptionCreatedAt != nil {
		merged.SubscriptionCreatedAt = cloneStringPointer(update.SubscriptionCreatedAt)
	}
	return merged
}
