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
// (Anthropic-Ratelimit-Unified-7d-* and -5h-*). Auths are ordered in four tiers:
//  1. auths with room left and a known future weekly reset, soonest reset first
//     (ties broken by the 5h reset, then by ID);
//  2. auths with no usable signals (never used, other providers, or a reset already in
//     the past, which means the window has started over), by ID;
//  3. draining auths, whose 5h utilization reached SessionDrainAt or weekly utilization
//     reached WeeklyDrainAt but neither window is full yet, lowest utilization first;
//  4. auths whose signals say a window is full until a future reset, soonest recovery first.
//
// Draining keeps new sessions off an auth that is about to hit its limit. Sessions already
// bound to it through session affinity stay, so their prompt cache stays warm until the
// limit is really reached.
//
// Cooldowns and disabled auths are filtered out before the tiers, as in the other selectors.
type SoonestResetSelector struct {
	// SessionDrainAt and WeeklyDrainAt are the 5h and weekly utilization (0-1) at which an
	// auth stops getting new sessions. Zero uses the default; 1 or more turns that window off.
	SessionDrainAt float64
	WeeklyDrainAt  float64
}

// Default utilization at which soonest-reset drains an auth, per window.
const (
	DefaultSoonestResetSessionDrainAt = 0.95
	DefaultSoonestResetWeeklyDrainAt  = 0.99
)

// soonestResetDrain holds the drain thresholds in effect for one ordering.
type soonestResetDrain struct {
	session float64
	weekly  float64
}

var defaultSoonestResetDrain = soonestResetDrain{
	session: DefaultSoonestResetSessionDrainAt,
	weekly:  DefaultSoonestResetWeeklyDrainAt,
}

const (
	soonestResetWeeklyPrefix  = "Anthropic-Ratelimit-Unified-7d-"
	soonestResetSessionPrefix = "Anthropic-Ratelimit-Unified-5h-"
)

const (
	soonestResetTierRoom = iota
	soonestResetTierUnknown
	soonestResetTierDraining
	soonestResetTierFull
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
	return soonestResetOrder(available, now, s.drain())[0], nil
}

func (s *SoonestResetSelector) drain() soonestResetDrain {
	drain := defaultSoonestResetDrain
	if s != nil && s.SessionDrainAt > 0 {
		drain.session = s.SessionDrainAt
	}
	if s != nil && s.WeeklyDrainAt > 0 {
		drain.weekly = s.WeeklyDrainAt
	}
	return drain
}

type soonestResetRank struct {
	auth        *Auth
	tier        int
	weeklyReset time.Time
	sessReset   time.Time
	recoverAt   time.Time
	utilization float64
}

// soonestResetOrder returns available sorted by preference. It does not modify available.
func soonestResetOrder(available []*Auth, now time.Time, drain soonestResetDrain) []*Auth {
	ranks := make([]soonestResetRank, 0, len(available))
	for _, auth := range available {
		ranks = append(ranks, rankSoonestReset(auth, now, drain))
	}
	sort.SliceStable(ranks, func(i, j int) bool {
		a, b := ranks[i], ranks[j]
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		switch a.tier {
		case soonestResetTierRoom:
			if !a.weeklyReset.Equal(b.weeklyReset) {
				return a.weeklyReset.Before(b.weeklyReset)
			}
			if !a.sessReset.Equal(b.sessReset) {
				return timeBeforeUnknownLast(a.sessReset, b.sessReset)
			}
		case soonestResetTierDraining:
			if a.utilization != b.utilization {
				return a.utilization < b.utilization
			}
		case soonestResetTierFull:
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

func rankSoonestReset(auth *Auth, now time.Time, drain soonestResetDrain) soonestResetRank {
	rank := soonestResetRank{auth: auth, tier: soonestResetTierUnknown}
	if auth == nil {
		return rank
	}
	signals := auth.Quota.Signals
	weekly := quotaWindowState(signals, soonestResetWeeklyPrefix, now)
	sess := quotaWindowState(signals, soonestResetSessionPrefix, now)
	if weekly.full || sess.full {
		rank.tier = soonestResetTierFull
		if weekly.full {
			rank.recoverAt = weekly.reset
		}
		if sess.full && (rank.recoverAt.IsZero() || sess.reset.After(rank.recoverAt)) {
			rank.recoverAt = sess.reset
		}
		return rank
	}
	if sess.utilization >= drain.session || weekly.utilization >= drain.weekly {
		rank.tier = soonestResetTierDraining
		rank.utilization = max(weekly.utilization, sess.utilization)
		return rank
	}
	if weekly.reset.IsZero() {
		return rank
	}
	rank.tier = soonestResetTierRoom
	rank.weeklyReset = weekly.reset
	rank.sessReset = sess.reset
	return rank
}

type quotaWindow struct {
	reset       time.Time
	full        bool
	utilization float64
}

// quotaWindowState returns the window's future reset, whether the window is full until
// that reset, and its utilization. A window with an unknown or past reset is zero.
func quotaWindowState(signals map[string]string, prefix string, now time.Time) quotaWindow {
	reset, ok := parseQuotaResetSignal(signals[prefix+"Reset"])
	if !ok || !reset.After(now) {
		return quotaWindow{}
	}
	window := quotaWindow{reset: reset}
	if utilization, errParse := strconv.ParseFloat(strings.TrimSpace(signals[prefix+"Utilization"]), 64); errParse == nil {
		window.utilization = utilization
	}
	window.full = window.utilization >= 1 || strings.EqualFold(strings.TrimSpace(signals[prefix+"Status"]), "rejected")
	return window
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
