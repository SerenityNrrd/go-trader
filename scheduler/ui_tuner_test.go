package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMergeStrategyTunerOverrides(t *testing.T) {
	base := StrategyConfig{
		ID:              "spot-btc",
		Type:            "spot",
		IntervalSeconds: 3600,
		OpenStrategy: StrategyRef{
			Name:   "sma",
			Params: map[string]interface{}{"period": 20},
		},
	}
	overrides := map[string]json.RawMessage{
		"interval_seconds":            json.RawMessage(`7200`),
		"open_strategy.params.period": json.RawMessage(`10`),
	}
	merged, err := mergeStrategyTunerOverrides(base, overrides)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if merged.IntervalSeconds != 7200 {
		t.Fatalf("interval_seconds = %d, want 7200", merged.IntervalSeconds)
	}
	if merged.OpenStrategy.Params["period"] != float64(10) && merged.OpenStrategy.Params["period"] != 10 {
		t.Fatalf("period = %v, want 10", merged.OpenStrategy.Params["period"])
	}
}

func TestBuildUIStrategyConfigFields(t *testing.T) {
	stop := 2.5
	sc := StrategyConfig{
		ID:              "hl-btc",
		Type:            "perps",
		Platform:        "hyperliquid",
		Args:            []string{"triple_ema", "BTC", "1h"},
		IntervalSeconds: 3600,
		Direction:       DirectionLong,
		Leverage:        5,
		StopLossATRMult: &stop,
		OpenStrategy:    StrategyRef{Name: "triple_ema", Params: map[string]interface{}{"fast_period": 8}},
	}
	defaults := map[string]interface{}{"fast_period": 12, "slow_period": 26}
	resp := buildUIStrategyConfig(sc, defaults, "", false)
	if resp.OpenStrategy.Params["fast_period"] != 8 {
		t.Fatalf("merged fast_period = %v, want 8", resp.OpenStrategy.Params["fast_period"])
	}
	foundRuntime := false
	foundParam := false
	for _, field := range resp.EditableFields {
		if field.Key == "leverage" {
			foundRuntime = true
		}
		if field.Key == "open_strategy.params.fast_period" {
			foundParam = true
		}
	}
	if !foundRuntime || !foundParam {
		t.Fatalf("editable fields missing runtime/param: %+v", resp.EditableFields)
	}
}

func TestApplyStrategyConfigPatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
  "strategies": [
    {
      "id": "spot-btc",
      "type": "spot",
      "platform": "binanceus",
      "args": ["sma", "BTC/USDT", "1h"],
      "open_strategy": {"name": "sma", "params": {"period": 20}}
    }
  ]
}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	merged := StrategyConfig{
		ID:              "spot-btc",
		Type:            "spot",
		IntervalSeconds: 7200,
		OpenStrategy: StrategyRef{
			Name:   "sma",
			Params: map[string]interface{}{"period": 10},
		},
	}
	restartRequired, err := applyStrategyConfigPatch(path, "spot-btc", merged)
	if err != nil {
		t.Fatalf("applyStrategyConfigPatch: %v", err)
	}
	if !restartRequired {
		t.Fatal("expected restartRequired=true")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var root struct {
		Strategies []map[string]interface{} `json:"strategies"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if len(root.Strategies) != 1 {
		t.Fatalf("strategies len = %d, want 1", len(root.Strategies))
	}
	if root.Strategies[0]["interval_seconds"] != float64(7200) {
		t.Fatalf("interval_seconds = %v, want 7200", root.Strategies[0]["interval_seconds"])
	}
	openRef := root.Strategies[0]["open_strategy"].(map[string]interface{})
	params := openRef["params"].(map[string]interface{})
	if params["period"] != float64(10) {
		t.Fatalf("period = %v, want 10", params["period"])
	}
}

func TestSimulateConfigPayloadOpenFallback(t *testing.T) {
	sc := StrategyConfig{
		Type:     "spot",
		Platform: "binanceus",
		Args:     []string{"sma", "BTC/USDT", "1h"},
	}
	payload := simulateConfigPayload(sc, nil)
	if payload["strategy"] != "sma" {
		t.Fatalf("strategy = %v, want sma", payload["strategy"])
	}
	openRef := payload["open_strategy"].(StrategyRef)
	if openRef.Name != "sma" {
		t.Fatalf("open_strategy.name = %q, want sma", openRef.Name)
	}
}
