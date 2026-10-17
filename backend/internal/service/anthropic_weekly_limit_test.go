//go:build unit

package service

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAnthropicWeeklyOverrideThreshold(t *testing.T) {
	now := time.Now()
	fiveHourReset := now.Add(time.Hour)
	account := &Account{
		ID: 1, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
		SessionWindowEnd: &fiveHourReset,
		Extra: map[string]any{
			AnthropicIgnoreWeeklyLimitExtraKey: true,
			"passive_usage_7d_utilization":     1.0,
			"passive_usage_7d_reset":           now.Add(3 * 24 * time.Hour).Unix(),
			"session_window_utilization":       0.2,
		},
	}
	thresholds := map[string]int{PlatformAnthropic: 90}
	require.False(t, EvaluateAccountSchedulingThreshold(account, thresholds, now).ShouldPause)
	require.True(t, account.IsSchedulable())
	account.Extra["session_window_utilization"] = 0.95
	decision := EvaluateAccountSchedulingThreshold(account, thresholds, now)
	require.True(t, decision.ShouldPause)
	require.Equal(t, "5h", decision.Window)
	account.Extra[AnthropicIgnoreWeeklyLimitExtraKey] = false
	require.Equal(t, "7d", EvaluateAccountSchedulingThreshold(account, thresholds, now).Window)

	account.Extra[AnthropicIgnoreWeeklyLimitExtraKey] = true
	account.Schedulable = false
	require.False(t, account.IsSchedulable(), "manual pause still applies")
	account.Schedulable = true
	account.Status = StatusError
	require.False(t, account.IsSchedulable(), "credential/account errors still apply")
}

func TestAnthropicWeeklyOverrideScope(t *testing.T) {
	for _, tc := range []struct {
		platform, typ string
		value         any
		want          bool
	}{
		{PlatformAnthropic, AccountTypeOAuth, true, true},
		{PlatformAnthropic, AccountTypeSetupToken, true, true},
		{PlatformAnthropic, AccountTypeAPIKey, true, false},
		{PlatformOpenAI, AccountTypeOAuth, true, false},
		{PlatformAnthropic, AccountTypeOAuth, false, false},
		{PlatformAnthropic, AccountTypeOAuth, "true", false},
	} {
		account := &Account{Platform: tc.platform, Type: tc.typ, Extra: map[string]any{AnthropicIgnoreWeeklyLimitExtraKey: tc.value}}
		require.Equal(t, tc.want, account.IgnoresAnthropicWeeklyLimit(), "%+v", tc)
	}
	var account *Account
	require.False(t, account.IgnoresAnthropicWeeklyLimit())
}

func TestAnthropicWeeklyOverride429(t *testing.T) {
	for _, direct := range []bool{false, true} {
		for _, tc := range []struct {
			name                                        string
			enabled, fiveHour, aggregateOnly, resetOnly bool
		}{
			{name: "disabled keeps weekly freeze"},
			{name: "enabled uses short cooldown", enabled: true},
			{name: "simultaneous 5h still blocks", enabled: true, fiveHour: true},
			{name: "aggregate weekly claim uses short cooldown", enabled: true, aggregateOnly: true},
			{name: "weekly reset alone cannot restore long freeze", enabled: true, resetOnly: true},
		} {
			t.Run(tc.name+"/direct="+strconv.FormatBool(direct), func(t *testing.T) {
				now := time.Now()
				weeklyReset := now.Add(3 * 24 * time.Hour).Truncate(time.Second)
				fiveHourReset := now.Add(2 * time.Hour).Truncate(time.Second)
				headers := http.Header{}
				headers.Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(weeklyReset.Unix(), 10))
				headers.Set("anthropic-ratelimit-unified-representative-claim", "seven_day")
				if !tc.aggregateOnly {
					headers.Set("anthropic-ratelimit-unified-7d-utilization", "1.02")
					headers.Set("anthropic-ratelimit-unified-7d-reset", strconv.FormatInt(weeklyReset.Unix(), 10))
					headers.Set("anthropic-ratelimit-unified-5h-utilization", "0.2")
					headers.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(fiveHourReset.Unix(), 10))
				}
				if tc.fiveHour {
					headers.Set("anthropic-ratelimit-unified-5h-status", "rejected")
				}
				if tc.resetOnly {
					headers = http.Header{}
					headers.Set("anthropic-ratelimit-unified-7d-reset", strconv.FormatInt(weeklyReset.Unix(), 10))
				}
				repo := &anthropicWindowLimitRepo{}
				blocker := &runtimeBlockRecorder{}
				svc := NewRateLimitService(repo, nil, nil, nil, nil)
				svc.SetAccountRuntimeBlocker(blocker)
				account := &Account{ID: 42, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
					Extra: map[string]any{AnthropicIgnoreWeeklyLimitExtraKey: tc.enabled},
					Credentials: map[string]any{"temp_unschedulable_enabled": true, "temp_unschedulable_rules": []any{
						map[string]any{"error_code": float64(429), "keywords": []any{"rate limit"}, "duration_minutes": float64(60)},
					}},
				}
				if direct {
					svc.handle429(context.Background(), account, headers, nil)
				} else {
					svc.HandleUpstreamError(context.Background(), account, 429, headers, []byte(`{"error":{"message":"rate limit"}}`))
				}
				require.Equal(t, 1, repo.rateLimitCalls)
				require.Zero(t, repo.tempUnschedCalls, "weekly override must not be re-paused by a broad 429 rule")
				require.Len(t, blocker.until, 1)
				require.Equal(t, repo.lastRateLimitReset, blocker.until[0], "runtime and persisted cooldowns must agree")
				if !tc.enabled {
					require.Equal(t, weeklyReset, repo.lastRateLimitReset)
				} else if tc.fiveHour {
					require.Equal(t, fiveHourReset, repo.lastRateLimitReset)
				} else {
					require.WithinDuration(t, now.Add(defaultRateLimit429CooldownSeconds*time.Second), repo.lastRateLimitReset, time.Second)
					require.Zero(t, repo.sessionWindowCalls, "weekly rejection must not poison the 5h window")
				}
			})
		}
	}
}

func TestAnthropicWeeklyOverrideKeepsFableLimit(t *testing.T) {
	now := time.Now()
	repo := &anthropicWindowLimitRepo{}
	svc := NewRateLimitService(repo, nil, nil, nil, nil)
	account := &Account{ID: 42, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
		Extra: map[string]any{AnthropicIgnoreWeeklyLimitExtraKey: true}}
	headers := fable429Headers(now.Add(time.Hour), now.Add(3*24*time.Hour))
	svc.HandleUpstreamError(context.Background(), account, 429, headers, nil)
	require.Equal(t, 1, repo.modelRateLimitCalls)
	require.Equal(t, anthropicFableRateLimitKey, repo.lastModelRateLimitScope)
	require.Zero(t, repo.rateLimitCalls, "Fable-only rejection must not cool down other models")
}

func TestAnthropicWeeklyOverrideFiveHourAggregate(t *testing.T) {
	resetAt := time.Now().Add(time.Hour).Truncate(time.Second)
	headers := http.Header{}
	headers.Set("anthropic-ratelimit-unified-7d-utilization", "1.0")
	headers.Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
	headers.Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(resetAt.Unix(), 10))
	repo := &anthropicWindowLimitRepo{}
	svc := NewRateLimitService(repo, nil, nil, nil, nil)
	account := &Account{ID: 42, Platform: PlatformAnthropic, Type: AccountTypeSetupToken,
		Extra: map[string]any{AnthropicIgnoreWeeklyLimitExtraKey: true}}
	svc.HandleUpstreamError(context.Background(), account, 429, headers, nil)
	require.Equal(t, 1, repo.rateLimitCalls)
	require.Equal(t, resetAt, repo.lastRateLimitReset)
}

type anthropicWeeklyExtendingRepo struct {
	anthropicWindowLimitRepo
	storedReset time.Time
	extendCalls int
}

func (r *anthropicWeeklyExtendingRepo) SetRateLimitedIfLater(_ context.Context, _ int64, resetAt time.Time) error {
	r.extendCalls++
	if resetAt.After(r.storedReset) {
		r.storedReset = resetAt
	}
	return nil
}

func (r *anthropicWeeklyExtendingRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	return &Account{ID: id, RateLimitResetAt: &r.storedReset}, nil
}

func TestAnthropicWeeklyOverrideDoesNotShortenConcurrentFiveHourCooldown(t *testing.T) {
	resetAt := time.Now().Add(2 * time.Hour)
	repo := &anthropicWeeklyExtendingRepo{storedReset: resetAt}
	svc := NewRateLimitService(repo, nil, nil, nil, nil)
	blocker := &runtimeBlockRecorder{}
	svc.SetAccountRuntimeBlocker(blocker)
	// This request was already in flight when another request persisted 5h exhaustion.
	staleAccount := &Account{ID: 42, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
		Extra: map[string]any{AnthropicIgnoreWeeklyLimitExtraKey: true}}
	headers := http.Header{}
	headers.Set("anthropic-ratelimit-unified-7d-utilization", "1.0")
	svc.HandleUpstreamError(context.Background(), staleAccount, 429, headers, nil)
	require.Equal(t, 1, repo.extendCalls)
	require.Zero(t, repo.rateLimitCalls, "must use the atomic extending write")
	require.Equal(t, resetAt, repo.storedReset)
	require.Equal(t, []time.Time{resetAt}, blocker.until)
}
