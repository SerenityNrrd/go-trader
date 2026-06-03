package main

import (
	"strings"
	"testing"
	"time"
)

func TestDiscordBackend(t *testing.T) {
	// No Discord backend present.
	mn := NewMultiNotifier()
	if got := mn.DiscordBackend(); got != nil {
		t.Fatalf("expected nil DiscordBackend on empty notifier, got %v", got)
	}

	// Discord backend present (zero-value *DiscordNotifier is fine for identity).
	d := &DiscordNotifier{}
	mn2 := NewMultiNotifier(notifierBackend{notifier: d})
	if got := mn2.DiscordBackend(); got != d {
		t.Fatalf("expected DiscordBackend to return the registered *DiscordNotifier, got %v", got)
	}
}

func TestAuthorizeCommand(t *testing.T) {
	const owner = "owner123"
	cases := []struct {
		name, invoker, guildID string
		wantOK                 bool
	}{
		{"status", "anyone", "guild1", true}, // read-only in guild OK
		{"status", "anyone", "", true},       // read-only in DM OK
		{"positions", "anyone", "guild1", true},
		{"logs", "anyone", "guild1", true},
		{"restart", owner, "", true},        // ops: owner in DM OK
		{"restart", owner, "guild1", false}, // ops: owner in guild rejected (must be DM)
		{"restart", "intruder", "", false},  // ops: non-owner in DM rejected
		{"backtest", owner, "", true},
		{"backtest", "intruder", "", false},
		{"unknown", owner, "", false}, // unknown command rejected
	}
	for _, c := range cases {
		ok, reason := authorizeCommand(c.name, c.invoker, c.guildID, owner)
		if ok != c.wantOK {
			t.Errorf("authorizeCommand(%q, %q, guild=%q) = %v (%q), want %v",
				c.name, c.invoker, c.guildID, ok, reason, c.wantOK)
		}
		if !ok && reason == "" {
			t.Errorf("authorizeCommand(%q,...) denied without a reason", c.name)
		}
	}
}

func TestFormatHealthResponse(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)

	never := formatHealthResponse(time.Time{}, 0, "v1", now)
	if !strings.Contains(never, "never") {
		t.Errorf("expected 'never' for zero last cycle, got: %s", never)
	}

	ok := formatHealthResponse(now.Add(-1*time.Minute), 42, "v1", now)
	if !strings.Contains(ok, "ok") || !strings.Contains(ok, "42") {
		t.Errorf("expected ok status with cycle count, got: %s", ok)
	}

	stale := formatHealthResponse(now.Add(-31*time.Minute), 42, "v1", now)
	if !strings.Contains(stale, "stale") {
		t.Errorf("expected stale status, got: %s", stale)
	}
}

func TestFormatStatusResponse(t *testing.T) {
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-a": {ID: "hl-a", Platform: "hyperliquid", Cash: 100,
			Positions: map[string]*Position{"BTC": {Symbol: "BTC", Quantity: 1, AvgCost: 50, Side: "long"}},
			Regime:    "trend_up"},
	}}
	prices := map[string]float64{"BTC": 60}
	got := formatStatusResponse(state, prices)
	if !strings.Contains(got, "positions=1") {
		t.Errorf("expected 1 position in status, got: %s", got)
	}
	if !strings.Contains(got, "regime=trend_up") {
		t.Errorf("expected regime in status, got: %s", got)
	}
}
