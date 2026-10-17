package service

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const AnthropicIgnoreWeeklyLimitExtraKey = "anthropic_ignore_weekly_limit"

// IgnoresAnthropicWeeklyLimit is an opt-in override for the shared Claude Code
// weekly window. It does not bypass 5h, model-specific, or authentication limits.
func (a *Account) IgnoresAnthropicWeeklyLimit() bool {
	return a != nil && a.IsAnthropicOAuthOrSetupToken() && a.getExtraBool(AnthropicIgnoreWeeklyLimitExtraKey)
}

func selectAnthropicAccountWindowLimit(account *Account, headers http.Header, now time.Time) *anthropicWindowLimit {
	if !account.IgnoresAnthropicWeeklyLimit() {
		return selectAnthropicExhaustedWindow(headers, now)
	}
	// A shared weekly override must not hide a simultaneous 5h exhaustion.
	claimIs5h := strings.EqualFold(strings.TrimSpace(headers.Get("anthropic-ratelimit-unified-representative-claim")), "five_hour")
	if isAnthropic5hRejected(headers) || isAnthropicWindowExceeded(headers, "5h") || claimIs5h {
		resetAt, ok := parseAnthropicWindowReset(headers, "5h", now)
		if !ok && claimIs5h {
			resetAt, ok = parseAnthropicResetTimestamp(headers.Get("anthropic-ratelimit-unified-reset"), now, 6*time.Hour)
		}
		if ok {
			return &anthropicWindowLimit{window: "5h", resetAt: resetAt, reason: "anthropic_5h_window_exhausted"}
		}
	}
	return nil
}

// applyAnthropicWeeklyOverride429 keeps a real rejection on the normal short
// cooldown, instead of inferring a days-long freeze from shared weekly metadata.
// Called before both custom 429 rules and the legacy per-window/aggregate parser.
func (s *RateLimitService) applyAnthropicWeeklyOverride429(ctx context.Context, account *Account, headers http.Header) bool {
	if !account.IgnoresAnthropicWeeklyLimit() {
		return false
	}
	hasWeeklySignal := isAnthropicWindowExceeded(headers, "7d") ||
		isAnthropicWindowRejected(headers, "7d") ||
		strings.TrimSpace(headers.Get("anthropic-ratelimit-unified-7d-reset")) != "" ||
		strings.EqualFold(strings.TrimSpace(headers.Get("anthropic-ratelimit-unified-representative-claim")), "seven_day")
	if !hasWeeklySignal {
		return false
	}
	if !s.persistAnthropicExhaustedWindowLimit(ctx, account, headers) {
		s.applyAnthropicWeeklyRetryCooldown(ctx, account)
	}
	return true
}

func (s *RateLimitService) applyAnthropicWeeklyRetryCooldown(ctx context.Context, account *Account) {
	cooldown, enabled := s.get429FallbackCooldown(ctx, account)
	if !enabled {
		return
	}
	resetAt := time.Now().Add(cooldown)
	if account.RateLimitResetAt != nil && account.RateLimitResetAt.After(resetAt) {
		resetAt = *account.RateLimitResetAt
	}
	// Preserve a real 5h cooldown written by another in-flight request. The
	// concrete repository's conditional update also covers stale account snapshots.
	if extender, ok := s.accountRepo.(interface {
		SetRateLimitedIfLater(context.Context, int64, time.Time) error
	}); ok {
		if err := extender.SetRateLimitedIfLater(ctx, account.ID, resetAt); err != nil {
			slog.Warn("anthropic_weekly_retry_cooldown_failed", "account_id", account.ID, "error", err)
			return
		}
		latest, err := s.accountRepo.GetByID(ctx, account.ID)
		if err != nil {
			slog.Warn("anthropic_weekly_retry_cooldown_reload_failed", "account_id", account.ID, "error", err)
			return
		}
		if latest == nil || latest.RateLimitResetAt == nil {
			return
		}
		resetAt = *latest.RateLimitResetAt
	} else if err := s.accountRepo.SetRateLimited(ctx, account.ID, resetAt); err != nil {
		slog.Warn("anthropic_weekly_retry_cooldown_failed", "account_id", account.ID, "error", err)
		return
	}
	s.notifyAccountSchedulingBlocked(account, resetAt, "anthropic_weekly_limit_override")
}
