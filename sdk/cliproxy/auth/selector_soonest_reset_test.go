package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func claudeQuotaSignals(weeklyUtil string, weeklyReset time.Time, sessionUtil string, sessionReset time.Time) QuotaState {
	signals := map[string]string{}
	if weeklyUtil != "" {
		signals["Anthropic-Ratelimit-Unified-7d-Utilization"] = weeklyUtil
		signals["Anthropic-Ratelimit-Unified-7d-Status"] = "allowed"
	}
	if !weeklyReset.IsZero() {
		signals["Anthropic-Ratelimit-Unified-7d-Reset"] = strconv.FormatInt(weeklyReset.Unix(), 10)
	}
	if sessionUtil != "" {
		signals["Anthropic-Ratelimit-Unified-5h-Utilization"] = sessionUtil
		signals["Anthropic-Ratelimit-Unified-5h-Status"] = "allowed"
	}
	if !sessionReset.IsZero() {
		signals["Anthropic-Ratelimit-Unified-5h-Reset"] = strconv.FormatInt(sessionReset.Unix(), 10)
	}
	return QuotaState{Signals: signals}
}

func soonestResetIDs(auths []*Auth) []string {
	ids := make([]string, len(auths))
	for i, auth := range auths {
		ids[i] = auth.ID
	}
	return ids
}

func TestSoonestResetOrder(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	hour := time.Hour
	day := 24 * time.Hour

	tests := []struct {
		name  string
		auths []*Auth
		want  []string
	}{
		{
			name: "soonest weekly reset first",
			auths: []*Auth{
				{ID: "sat", Quota: claudeQuotaSignals("0.07", now.Add(5*day), "0", now.Add(2*hour))},
				{ID: "tue", Quota: claudeQuotaSignals("0.40", now.Add(1*day), "0", now.Add(2*hour))},
				{ID: "fri", Quota: claudeQuotaSignals("0.90", now.Add(4*day), "0", now.Add(2*hour))},
			},
			want: []string{"tue", "fri", "sat"},
		},
		{
			name: "full weekly window goes last even when it resets soonest",
			auths: []*Auth{
				{ID: "full", Quota: claudeQuotaSignals("1.0", now.Add(1*hour), "0", now.Add(1*hour))},
				{ID: "room", Quota: claudeQuotaSignals("0.5", now.Add(3*day), "0", now.Add(1*hour))},
			},
			want: []string{"room", "full"},
		},
		{
			name: "full session window goes last",
			auths: []*Auth{
				{ID: "session-full", Quota: claudeQuotaSignals("0.1", now.Add(1*day), "1.0", now.Add(3*hour))},
				{ID: "ok", Quota: claudeQuotaSignals("0.1", now.Add(2*day), "0.2", now.Add(3*hour))},
			},
			want: []string{"ok", "session-full"},
		},
		{
			name: "unknown signals after known room and before full",
			auths: []*Auth{
				{ID: "full", Quota: claudeQuotaSignals("1.0", now.Add(2*day), "", time.Time{})},
				{ID: "unknown"},
				{ID: "known", Quota: claudeQuotaSignals("0.2", now.Add(6*day), "", time.Time{})},
			},
			want: []string{"known", "unknown", "full"},
		},
		{
			name: "reset in the past counts as unknown",
			auths: []*Auth{
				{ID: "stale-full", Quota: claudeQuotaSignals("1.0", now.Add(-hour), "", time.Time{})},
				{ID: "known", Quota: claudeQuotaSignals("0.2", now.Add(6*day), "", time.Time{})},
			},
			want: []string{"known", "stale-full"},
		},
		{
			name: "session reset breaks a weekly tie",
			auths: []*Auth{
				{ID: "late", Quota: claudeQuotaSignals("0.2", now.Add(day), "0.1", now.Add(4*hour))},
				{ID: "early", Quota: claudeQuotaSignals("0.2", now.Add(day), "0.1", now.Add(1*hour))},
			},
			want: []string{"early", "late"},
		},
		{
			name: "full auths ordered by soonest recovery",
			auths: []*Auth{
				{ID: "later", Quota: claudeQuotaSignals("1.0", now.Add(3*day), "", time.Time{})},
				{ID: "sooner", Quota: claudeQuotaSignals("1.0", now.Add(1*day), "", time.Time{})},
			},
			want: []string{"sooner", "later"},
		},
		{
			name:  "no signals falls back to ID order",
			auths: []*Auth{{ID: "b"}, {ID: "a"}, {ID: "c"}},
			want:  []string{"a", "b", "c"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := soonestResetIDs(soonestResetOrder(tt.auths, now))
			if len(got) != len(tt.want) {
				t.Fatalf("order = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("order = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestSoonestResetOrder_RejectedStatusIsFull(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	rejected := claudeQuotaSignals("0.5", now.Add(time.Hour), "", time.Time{})
	rejected.Signals["Anthropic-Ratelimit-Unified-7d-Status"] = "rejected"
	auths := []*Auth{
		{ID: "rejected", Quota: rejected},
		{ID: "ok", Quota: claudeQuotaSignals("0.5", now.Add(48*time.Hour), "", time.Time{})},
	}
	got := soonestResetIDs(soonestResetOrder(auths, now))
	if got[0] != "ok" {
		t.Fatalf("order = %v, want ok first", got)
	}
}

func TestSoonestResetSelectorPick(t *testing.T) {
	t.Parallel()

	now := time.Now()
	selector := &SoonestResetSelector{}
	auths := []*Auth{
		{ID: "a", Quota: claudeQuotaSignals("0.1", now.Add(96*time.Hour), "0", now.Add(time.Hour))},
		{ID: "b", Quota: claudeQuotaSignals("0.6", now.Add(20*time.Hour), "0", now.Add(time.Hour))},
	}
	got, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got.ID != "b" {
		t.Fatalf("Pick() auth.ID = %q, want b", got.ID)
	}
}

func TestSessionAffinitySelector_SoonestResetFallbackBindsAndSticks(t *testing.T) {
	t.Parallel()

	now := time.Now()
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &SoonestResetSelector{},
		TTL:      time.Hour,
	})
	defer selector.Stop()

	auths := []*Auth{
		{ID: "late", Quota: claudeQuotaSignals("0.1", now.Add(96*time.Hour), "0", now.Add(time.Hour))},
		{ID: "soon", Quota: claudeQuotaSignals("0.5", now.Add(20*time.Hour), "0", now.Add(time.Hour))},
	}
	opts := cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"metadata":{"user_id":"user_xxx_account__session_11111111-2222-3333-4444-555555555555"}}`),
	}

	first, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", opts, auths)
	if err != nil {
		t.Fatalf("first Pick() error = %v", err)
	}
	if first.ID != "soon" {
		t.Fatalf("first Pick() auth.ID = %q, want soon", first.ID)
	}

	// "late" now resets sooner, but the bound session keeps its auth so its cache stays warm.
	auths[0].Quota = claudeQuotaSignals("0.1", now.Add(2*time.Hour), "0", now.Add(time.Hour))
	second, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", opts, auths)
	if err != nil {
		t.Fatalf("second Pick() error = %v", err)
	}
	if second.ID != "soon" {
		t.Fatalf("second Pick() auth.ID = %q, want soon (affinity)", second.ID)
	}
}
