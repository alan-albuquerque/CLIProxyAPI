package auth

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// SoonestResetSelector picks the available auth whose weekly rate-limit window resets first,
// so quota that is about to reset is used before quota that would carry over.
//
// It reads the Claude unified rate-limit signals observed on each auth's last response
// (Anthropic-Ratelimit-Unified-7d-* and -5h-*). Auths are ordered in three tiers:
//  1. auths with room left and a known future weekly reset, soonest reset first
//     (ties broken by the 5h reset, then by ID);
//  2. auths with no usable signals (never used, other providers, or a reset already in
//     the past, which means the window has started over), by ID;
//  3. auths whose signals say a window is full until a future reset, soonest recovery first.
//
// Cooldowns and disabled auths are filtered out before the tiers, as in the other selectors.
type SoonestResetSelector struct{}

const (
	soonestResetWeeklyPrefix  = "Anthropic-Ratelimit-Unified-7d-"
	soonestResetSessionPrefix = "Anthropic-Ratelimit-Unified-5h-"
)

// Pick selects the auth whose weekly window resets soonest among those with room left.
func (s *SoonestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	return soonestResetOrder(available, now)[0], nil
}

type soonestResetRank struct {
	auth        *Auth
	tier        int
	weeklyReset time.Time
	sessReset   time.Time
	recoverAt   time.Time
}

// soonestResetOrder returns available sorted by preference. It does not modify available.
func soonestResetOrder(available []*Auth, now time.Time) []*Auth {
	ranks := make([]soonestResetRank, 0, len(available))
	for _, auth := range available {
		ranks = append(ranks, rankSoonestReset(auth, now))
	}
	sort.SliceStable(ranks, func(i, j int) bool {
		a, b := ranks[i], ranks[j]
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		switch a.tier {
		case 0:
			if !a.weeklyReset.Equal(b.weeklyReset) {
				return a.weeklyReset.Before(b.weeklyReset)
			}
			if !a.sessReset.Equal(b.sessReset) {
				return timeBeforeUnknownLast(a.sessReset, b.sessReset)
			}
		case 2:
			if !a.recoverAt.Equal(b.recoverAt) {
				return a.recoverAt.Before(b.recoverAt)
			}
		}
		return a.auth.ID < b.auth.ID
	})
	ordered := make([]*Auth, len(ranks))
	for i, rank := range ranks {
		ordered[i] = rank.auth
	}
	return ordered
}

func rankSoonestReset(auth *Auth, now time.Time) soonestResetRank {
	rank := soonestResetRank{auth: auth, tier: 1}
	if auth == nil {
		return rank
	}
	signals := auth.Quota.Signals
	weeklyReset, weeklyFull := quotaWindowState(signals, soonestResetWeeklyPrefix, now)
	sessReset, sessFull := quotaWindowState(signals, soonestResetSessionPrefix, now)
	if weeklyFull || sessFull {
		rank.tier = 2
		if weeklyFull {
			rank.recoverAt = weeklyReset
		}
		if sessFull && (rank.recoverAt.IsZero() || sessReset.After(rank.recoverAt)) {
			rank.recoverAt = sessReset
		}
		return rank
	}
	if weeklyReset.IsZero() {
		return rank
	}
	rank.tier = 0
	rank.weeklyReset = weeklyReset
	rank.sessReset = sessReset
	return rank
}

// quotaWindowState returns the window's future reset (zero when unknown or already past)
// and whether the window is full until that reset.
func quotaWindowState(signals map[string]string, prefix string, now time.Time) (time.Time, bool) {
	reset, ok := parseQuotaResetSignal(signals[prefix+"Reset"])
	if !ok || !reset.After(now) {
		return time.Time{}, false
	}
	if strings.EqualFold(strings.TrimSpace(signals[prefix+"Status"]), "rejected") {
		return reset, true
	}
	if utilization, errParse := strconv.ParseFloat(strings.TrimSpace(signals[prefix+"Utilization"]), 64); errParse == nil && utilization >= 1 {
		return reset, true
	}
	return reset, false
}

// parseQuotaResetSignal parses a reset value given as unix seconds or RFC 3339.
func parseQuotaResetSignal(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if seconds, errParse := strconv.ParseInt(value, 10, 64); errParse == nil {
		if seconds <= 0 {
			return time.Time{}, false
		}
		return time.Unix(seconds, 0), true
	}
	if parsed, errParse := time.Parse(time.RFC3339, value); errParse == nil {
		return parsed, true
	}
	return time.Time{}, false
}

func timeBeforeUnknownLast(a, b time.Time) bool {
	if a.IsZero() {
		return false
	}
	if b.IsZero() {
		return true
	}
	return a.Before(b)
}
