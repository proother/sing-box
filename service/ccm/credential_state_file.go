package ccm

import (
	"encoding/json"
	"os"
)

// persistedState holds profile data fetched from /api/oauth/profile,
// persisted to state_path so it survives restarts without re-fetching.
//
// Claude Code (@anthropic-ai/claude-code @2.1.81) stores equivalent data in
// its config file (~/.claude/.config.json) under the oauthAccount key:
//
//	ref: cli.js fP6() / storeOAuthAccountInfo — writes accountUuid, billingType, etc.
//	ref: cli.js P8() — reads config from $CLAUDE_CONFIG_DIR/.config.json
type persistedState struct {
	AccountUUID   string `json:"account_uuid,omitempty"`
	AccountType   string `json:"account_type,omitempty"`
	RateLimitTier string `json:"rate_limit_tier,omitempty"`
}

func (c *defaultCredential) loadPersistedState() {
	if c.statePath == "" {
		return
	}
	data, err := os.ReadFile(c.statePath)
	if err != nil {
		return
	}
	var state persistedState
	err = json.Unmarshal(data, &state)
	if err != nil {
		return
	}
	c.stateAccess.Lock()
	if state.AccountUUID != "" {
		c.state.accountUUID = state.AccountUUID
	}
	if state.AccountType != "" {
		c.state.accountType = state.AccountType
	}
	if state.RateLimitTier != "" {
		c.state.rateLimitTier = state.RateLimitTier
	}
	c.stateAccess.Unlock()
}

func (c *defaultCredential) savePersistedState() {
	if c.statePath == "" {
		return
	}
	c.stateAccess.RLock()
	state := persistedState{
		AccountUUID:   c.state.accountUUID,
		AccountType:   c.state.accountType,
		RateLimitTier: c.state.rateLimitTier,
	}
	c.stateAccess.RUnlock()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(c.statePath, data, 0o600)
}
