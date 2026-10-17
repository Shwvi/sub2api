//go:build unit

package repository

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSchedulerMetadataAnthropicWeeklyOverride(t *testing.T) {
	now := time.Now()
	account := service.Account{ID: 1, Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth,
		Status: service.StatusActive, Schedulable: true, Extra: map[string]any{
			service.AnthropicIgnoreWeeklyLimitExtraKey: true,
			"passive_usage_7d_utilization":             1.0,
			"passive_usage_7d_reset":                   now.Add(3 * 24 * time.Hour).Unix(),
		}}
	_, metadata, err := marshalSchedulerCacheAccount(account)
	require.NoError(t, err)
	var cached service.Account
	require.NoError(t, json.Unmarshal(metadata, &cached))
	require.True(t, cached.IgnoresAnthropicWeeklyLimit())
	thresholds := map[string]int{service.PlatformAnthropic: 90}
	require.False(t, service.EvaluateAccountSchedulingThreshold(&cached, thresholds, now).ShouldPause)
	cached.Extra[service.AnthropicIgnoreWeeklyLimitExtraKey] = false
	require.True(t, service.EvaluateAccountSchedulingThreshold(&cached, thresholds, now).ShouldPause)
}
