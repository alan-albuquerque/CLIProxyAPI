package cliproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

const (
	claudeUsageURL                    = "https://api.anthropic.com/api/oauth/usage"
	claudeUsageBeta                   = "oauth-2025-04-20"
	defaultClaudeUsageRefreshInterval = 5 * time.Minute
	claudeUsageRequestTimeout         = 10 * time.Second
	claudeUsageMaxBodyBytes           = 1 << 20
)

// claudeUsageRefresher keeps the quota signals of Claude OAuth credentials
// fresh by polling Anthropic's usage endpoint, so selectors that rank by quota
// work after a restart and for idle credentials.
type claudeUsageRefresher struct {
	manager  *coreauth.Manager
	config   func() *config.Config
	usageURL string
	now      func() time.Time
}

func newClaudeUsageRefresher(manager *coreauth.Manager, cfg func() *config.Config) *claudeUsageRefresher {
	return &claudeUsageRefresher{manager: manager, config: cfg, usageURL: claudeUsageURL, now: time.Now}
}

// claudeUsageRefreshInterval returns the configured poll interval; zero or
// negative means polling is disabled.
func claudeUsageRefreshInterval(cfg *config.Config) time.Duration {
	if cfg == nil {
		return defaultClaudeUsageRefreshInterval
	}
	raw := strings.TrimSpace(cfg.Routing.ClaudeUsageRefreshInterval)
	if raw == "" {
		return defaultClaudeUsageRefreshInterval
	}
	interval, errParse := time.ParseDuration(raw)
	if errParse != nil {
		log.Warnf("invalid routing.claude-usage-refresh-interval %q, using %s", raw, defaultClaudeUsageRefreshInterval)
		return defaultClaudeUsageRefreshInterval
	}
	return interval
}

// run refreshes all credentials immediately and then every interval until ctx is done.
func (r *claudeUsageRefresher) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		r.refreshAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// refreshAll polls each eligible credential in turn; one failure does not stop the others.
func (r *claudeUsageRefresher) refreshAll(ctx context.Context) {
	if r == nil || r.manager == nil {
		return
	}
	for _, auth := range r.manager.List() {
		if ctx.Err() != nil {
			return
		}
		if !isClaudeOAuthAuth(auth) {
			continue
		}
		if errRefresh := r.refreshOne(ctx, auth); errRefresh != nil {
			log.WithField("auth_id", auth.ID).Warnf("claude usage refresh failed: %v", errRefresh)
		}
	}
}

func isClaudeOAuthAuth(auth *coreauth.Auth) bool {
	return auth != nil && !auth.Disabled && auth.Status != coreauth.StatusDisabled &&
		strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") &&
		auth.AuthKind() == coreauth.AuthKindOAuth
}

func (r *claudeUsageRefresher) refreshOne(ctx context.Context, auth *coreauth.Auth) error {
	token, _ := auth.Metadata["access_token"].(string)
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("no access token")
	}
	startedAt := r.now()
	// Expired tokens are left to the manager's auto-refresh loop; the next poll uses the new token.
	if expiresAt, ok := auth.ExpirationTime(); ok && !expiresAt.After(startedAt) {
		log.WithField("auth_id", auth.ID).Debug("claude usage refresh skipped: access token expired")
		return nil
	}

	reqCtx, cancel := context.WithTimeout(ctx, claudeUsageRequestTimeout)
	defer cancel()
	req, errReq := http.NewRequestWithContext(reqCtx, http.MethodGet, r.usageURL, nil)
	if errReq != nil {
		return errReq
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", claudeUsageBeta)
	req.Header.Set("Accept", "application/json")

	var cfg *config.Config
	if r.config != nil {
		cfg = r.config()
	}
	client := helps.NewUtlsHTTPClient(reqCtx, cfg, auth, 0)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("claude usage refresh: close response body: %v", errClose)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("usage endpoint returned status %d", resp.StatusCode)
	}
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, claudeUsageMaxBodyBytes))
	if errRead != nil {
		return errRead
	}
	signals, allowed, errSignals := claudeUsageSignals(body)
	if errSignals != nil {
		return errSignals
	}
	// A failed merge means a response newer than this poll was observed; it may
	// be the 429 that set the cooldown, so only a fresh poll may clear one.
	if !r.manager.MergeQuotaSignals(auth.ID, signals, startedAt) || !allowed {
		return nil
	}
	cleared, errClear := r.manager.ClearRecoveredCredentialQuota(ctx, auth.ID)
	if cleared {
		log.WithField("auth_id", auth.ID).Info("claude usage shows every window has room; cleared quota cooldown")
	}
	return errClear
}

type claudeUsageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

// claudeUsageSignals maps a usage response to the unified rate-limit signals
// that Claude responses carry as Anthropic-Ratelimit-Unified-* headers.
// allowed reports whether at least one rate-limit window has a utilization and
// every one that does is below 100%. Rate-limit windows are the five_hour* and
// seven_day* keys, which include per-model weekly limits such as
// seven_day_opus; extra (paid) usage is not a rate limit and is ignored.
func claudeUsageSignals(body []byte) (signals map[string]string, allowed bool, err error) {
	var usage map[string]json.RawMessage
	if errDecode := json.Unmarshal(body, &usage); errDecode != nil {
		return nil, false, fmt.Errorf("decode usage response: %w", errDecode)
	}
	signals = make(map[string]string)
	reported := false
	allowed = true
	for key, raw := range usage {
		if !strings.HasPrefix(key, "five_hour") && !strings.HasPrefix(key, "seven_day") {
			continue
		}
		var window *claudeUsageWindow
		if errWindow := json.Unmarshal(raw, &window); errWindow != nil {
			// An unknown shape cannot prove the window has room.
			allowed = false
			continue
		}
		switch key {
		case "five_hour":
			addClaudeUsageWindow(signals, "Anthropic-Ratelimit-Unified-5h-", window)
		case "seven_day":
			addClaudeUsageWindow(signals, "Anthropic-Ratelimit-Unified-7d-", window)
		}
		if window == nil || window.Utilization == nil {
			continue
		}
		reported = true
		if *window.Utilization >= 100 {
			allowed = false
		}
	}
	return signals, reported && allowed, nil
}

func addClaudeUsageWindow(signals map[string]string, prefix string, window *claudeUsageWindow) {
	if window == nil {
		return
	}
	if window.Utilization != nil {
		percent := *window.Utilization
		signals[prefix+"Utilization"] = strconv.FormatFloat(percent/100, 'f', -1, 64)
		status := "allowed"
		if percent >= 100 {
			status = "rejected"
		}
		signals[prefix+"Status"] = status
	}
	if resetsAt, errParse := time.Parse(time.RFC3339, strings.TrimSpace(window.ResetsAt)); errParse == nil {
		signals[prefix+"Reset"] = strconv.FormatInt(resetsAt.Unix(), 10)
	}
}
