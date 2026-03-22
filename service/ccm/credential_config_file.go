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
	AccountUUID string `json:"accountUuid"`
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
		filepath.Join(claudeDirectory, ".claude.json"),
		filepath.Join(filepath.Dir(claudeDirectory), ".claude.json"),
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
