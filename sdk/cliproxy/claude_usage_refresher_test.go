package cliproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestClaudeUsageSignals(t *testing.T) {
	fiveHourReset := strconv.FormatInt(time.Date(2026, 10, 27, 5, 10, 0, 0, time.UTC).Unix(), 10)
	sevenDayReset := strconv.FormatInt(time.Date(2026, 10, 31, 19, 0, 0, 0, time.UTC).Unix(), 10)
	tests := []struct {
		name string
		body string
		want map[string]string
	}{
		{
			name: "both windows",
			body: `{"five_hour":{"utilization":0.0,"resets_at":"2026-10-27T05:10:00.452299+00:00"},
				"seven_day":{"utilization":7.0,"resets_at":"2026-10-31T19:00:00.452320+00:00"},"extra":{"utilization":50}}`,
			want: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": "0",
				"Anthropic-Ratelimit-Unified-5h-Status":      "allowed",
				"Anthropic-Ratelimit-Unified-5h-Reset":       fiveHourReset,
				"Anthropic-Ratelimit-Unified-7d-Utilization": "0.07",
				"Anthropic-Ratelimit-Unified-7d-Status":      "allowed",
				"Anthropic-Ratelimit-Unified-7d-Reset":       sevenDayReset,
			},
		},
		{
			name: "null window and full window",
			body: `{"five_hour":null,"seven_day":{"utilization":100,"resets_at":"2026-10-31T19:00:00.452320+00:00"}}`,
			want: map[string]string{
				"Anthropic-Ratelimit-Unified-7d-Utilization": "1",
				"Anthropic-Ratelimit-Unified-7d-Status":      "rejected",
				"Anthropic-Ratelimit-Unified-7d-Reset":       sevenDayReset,
			},
		},
		{
			name: "both null",
			body: `{"five_hour":null,"seven_day":null}`,
			want: map[string]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, errSignals := claudeUsageSignals([]byte(tt.body))
			if errSignals != nil {
				t.Fatalf("claudeUsageSignals() error = %v", errSignals)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("claudeUsageSignals() = %#v, want %#v", got, tt.want)
			}
		})
	}
	if _, errSignals := claudeUsageSignals([]byte("not json")); errSignals == nil {
		t.Fatal("claudeUsageSignals() accepted invalid JSON")
	}
}

func TestClaudeUsageRefreshInterval(t *testing.T) {
	tests := map[string]time.Duration{
		"":      defaultClaudeUsageRefreshInterval,
		"bogus": defaultClaudeUsageRefreshInterval,
		"0":     0,
		"-1m":   -time.Minute,
		"90s":   90 * time.Second,
	}
	for raw, want := range tests {
		cfg := &config.Config{}
		cfg.Routing.ClaudeUsageRefreshInterval = raw
		if got := claudeUsageRefreshInterval(cfg); got != want {
			t.Errorf("claudeUsageRefreshInterval(%q) = %s, want %s", raw, got, want)
		}
	}
}

func TestClaudeUsageRefresherUpdatesOnlyEligibleClaudeOAuthAuths(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	var seenTokens []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		seenTokens = append(seenTokens, auth)
		mu.Unlock()
		if r.URL.Path != "/api/oauth/usage" || r.Header.Get("anthropic-beta") != claudeUsageBeta {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if auth == "Bearer tok-fail" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":12.5,"resets_at":"2026-10-26T15:00:00+00:00"},
			"seven_day":{"utilization":100,"resets_at":"2026-10-30T00:00:00+00:00"}}`))
	}))
	defer server.Close()

	ctx := context.Background()
	manager := coreauth.NewManager(nil, nil, nil)
	future := now.Add(time.Hour).Format(time.RFC3339)
	auths := []*coreauth.Auth{
		{ID: "claude-ok", Provider: "claude", Metadata: map[string]any{"access_token": "tok-ok", "expired": future}},
		{ID: "claude-fail", Provider: "claude", Metadata: map[string]any{"access_token": "tok-fail", "expired": future}},
		{ID: "claude-expired", Provider: "claude", Metadata: map[string]any{"access_token": "tok-expired", "expired": now.Add(-time.Minute).Format(time.RFC3339)}},
		{ID: "claude-disabled", Provider: "claude", Disabled: true, Status: coreauth.StatusDisabled, Metadata: map[string]any{"access_token": "tok-disabled"}},
		{ID: "claude-api-key", Provider: "claude", Attributes: map[string]string{"api_key": "tok-api-key"}},
		{ID: "codex-oauth", Provider: "codex", Metadata: map[string]any{"access_token": "tok-codex"}},
	}
	for _, auth := range auths {
		if _, errRegister := manager.Register(coreauth.WithSkipPersist(ctx), auth); errRegister != nil {
			t.Fatalf("Register(%s): %v", auth.ID, errRegister)
		}
	}

	refresher := newClaudeUsageRefresher(manager, func() *config.Config { return &config.Config{} })
	refresher.usageURL = server.URL + "/api/oauth/usage"
	refresher.now = func() time.Time { return now }
	refresher.refreshAll(ctx)

	got, ok := manager.GetByID("claude-ok")
	if !ok {
		t.Fatal("claude-ok not found")
	}
	if !got.Quota.ObservedAt.Equal(now) {
		t.Fatalf("ObservedAt = %v, want %v", got.Quota.ObservedAt, now)
	}
	want := map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.125",
		"Anthropic-Ratelimit-Unified-5h-Status":      "allowed",
		"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(time.Date(2026, 10, 26, 15, 0, 0, 0, time.UTC).Unix(), 10),
		"Anthropic-Ratelimit-Unified-7d-Utilization": "1",
		"Anthropic-Ratelimit-Unified-7d-Status":      "rejected",
		"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC).Unix(), 10),
	}
	if !reflect.DeepEqual(got.Quota.Signals, want) {
		t.Fatalf("Signals = %#v, want %#v", got.Quota.Signals, want)
	}
	if got.Quota.Exceeded || !got.Quota.NextRecoverAt.IsZero() {
		t.Fatalf("cooldown fields changed: %#v", got.Quota)
	}

	for _, id := range []string{"claude-fail", "claude-expired", "claude-disabled", "claude-api-key", "codex-oauth"} {
		other, okOther := manager.GetByID(id)
		if !okOther {
			t.Fatalf("%s not found", id)
		}
		if len(other.Quota.Signals) != 0 || !other.Quota.ObservedAt.IsZero() {
			t.Fatalf("%s quota = %#v, want untouched", id, other.Quota)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	called := map[string]bool{}
	for _, token := range seenTokens {
		called[token] = true
	}
	if len(seenTokens) != 2 || !called["Bearer tok-ok"] || !called["Bearer tok-fail"] {
		t.Fatalf("usage endpoint called with %v, want only tok-ok and tok-fail", seenTokens)
	}
}
