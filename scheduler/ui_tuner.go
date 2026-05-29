package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
)

type UIEditableField struct {
	Key     string      `json:"key"`
	Label   string      `json:"label"`
	Type    string      `json:"type"`
	Value   interface{} `json:"value"`
	Default interface{} `json:"default,omitempty"`
	Options []string    `json:"options,omitempty"`
	Group   string      `json:"group"`
}

type UIStrategyConfigResponse struct {
	StrategyID           string                 `json:"strategy_id"`
	Type                 string                 `json:"type"`
	Platform             string                 `json:"platform"`
	Symbol               string                 `json:"symbol"`
	Timeframe            string                 `json:"timeframe"`
	OpenStrategy         StrategyRef            `json:"open_strategy"`
	CloseStrategies      []StrategyRef          `json:"close_strategies,omitempty"`
	IntervalSeconds      int                    `json:"interval_seconds,omitempty"`
	Direction            string                 `json:"direction,omitempty"`
	InvertSignal         bool                   `json:"invert_signal,omitempty"`
	Leverage             float64                `json:"leverage,omitempty"`
	HTFFilter            bool                   `json:"htf_filter,omitempty"`
	AllowedRegimes       []string               `json:"allowed_regimes,omitempty"`
	StopLossPct          *float64               `json:"stop_loss_pct,omitempty"`
	StopLossATRMult      *float64               `json:"stop_loss_atr_mult,omitempty"`
	DefaultParams        map[string]interface{} `json:"default_params"`
	EditableFields       []UIEditableField      `json:"editable_fields"`
	HasOpenPosition      bool                   `json:"has_open_position"`
	ApplyRequiresRestart bool                   `json:"apply_requires_restart"`
}

type UISimulateRequest struct {
	Overrides map[string]json.RawMessage `json:"overrides"`
	Limit     int                        `json:"limit"`
}

type UISimulateResponse struct {
	StrategyID       string          `json:"strategy_id"`
	Source           string          `json:"source,omitempty"`
	LiveMarkers      []UITradeMarker `json:"live_markers"`
	SimulatedMarkers []UITradeMarker `json:"simulated_markers"`
	Error            string          `json:"error,omitempty"`
}

type UIApplyConfigRequest struct {
	Overrides map[string]json.RawMessage `json:"overrides"`
}

type UIApplyConfigResponse struct {
	OK              bool   `json:"ok"`
	RestartRequired bool   `json:"restart_required"`
	Message         string `json:"message"`
}

func (ss *StatusServer) SetConfigContext(configPath string, regime *RegimeConfig) {
	if ss == nil {
		return
	}
	ss.configPath = configPath
	ss.regime = regime
}

func (ss *StatusServer) handleAPIStrategyConfig(w http.ResponseWriter, r *http.Request, id string) {
	sc, ok := ss.strategyConfig(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "strategy not found")
		return
	}
	defaults, desc, err := fetchStrategyDefaultParams(sc)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	hasOpen := ss.strategyHasOpenPosition(id)
	resp := buildUIStrategyConfig(sc, defaults, desc, hasOpen)
	writeJSON(w, resp)
}

func (ss *StatusServer) handleAPIStrategySimulate(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	sc, ok := ss.strategyConfig(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "strategy not found")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "failed to read body")
		return
	}
	var req UISimulateRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid json body")
			return
		}
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 400
	}
	if limit > 1000 {
		limit = 1000
	}

	liveCfg := sc
	simCfg, err := mergeStrategyTunerOverrides(sc, req.Overrides)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	from, to, _ := parseUITimeQuery(r)
	candleReq := UICandleRequest{Strategy: sc, From: from, To: to, Limit: limit}
	candles, source, err := ss.fetchUICandlesForSimulate(candleReq)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}

	livePayload := simulateConfigPayload(liveCfg, ss.regime)
	simPayload := simulateConfigPayload(simCfg, ss.regime)
	markersByLabel, simErr := runStrategySimulate(candles, map[string]map[string]interface{}{
		"live":      livePayload,
		"simulated": simPayload,
	})
	if simErr != nil {
		writeJSONError(w, http.StatusBadGateway, simErr.Error())
		return
	}

	writeJSON(w, UISimulateResponse{
		StrategyID:       id,
		Source:           source,
		LiveMarkers:      markersByLabel["live"],
		SimulatedMarkers: markersByLabel["simulated"],
	})
}

func (ss *StatusServer) handleAPIStrategyApplyConfig(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if strings.TrimSpace(ss.configPath) == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "config path not configured")
		return
	}
	sc, ok := ss.strategyConfig(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "strategy not found")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "failed to read body")
		return
	}
	var req UIApplyConfigRequest
	if len(body) == 0 {
		writeJSONError(w, http.StatusBadRequest, "empty body")
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	merged, err := mergeStrategyTunerOverrides(sc, req.Overrides)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	restartRequired, err := applyStrategyConfigPatch(ss.configPath, id, merged)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	msg := "Config updated."
	if restartRequired {
		msg += " Restart go-trader to apply indicator/script changes; some runtime fields may hot-reload on SIGHUP when flat."
	} else {
		msg += " Send SIGHUP or wait for the next reload to pick up changes."
	}
	writeJSON(w, UIApplyConfigResponse{
		OK:              true,
		RestartRequired: restartRequired,
		Message:         msg,
	})
}

func (ss *StatusServer) fetchUICandlesForSimulate(req UICandleRequest) ([]UICandle, string, error) {
	cacheKey := req.CacheKey()
	if ss.candleCache != nil {
		if cached, ok := ss.candleCache.Get(cacheKey); ok {
			return cached.Candles, cached.Source + ":cached", nil
		}
	}
	if ss.candleFetcher == nil {
		return nil, "", fmt.Errorf("candle fetcher unavailable")
	}
	candles, source, err := ss.candleFetcher(req)
	if err != nil {
		return nil, "", err
	}
	if ss.candleCache != nil {
		ss.candleCache.Set(cacheKey, UICandleResponse{Candles: candles, Source: source})
	}
	return candles, source, nil
}

func (ss *StatusServer) strategyHasOpenPosition(id string) bool {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	strat := ss.state.Strategies[id]
	if strat == nil {
		return false
	}
	for _, pos := range strat.Positions {
		if pos != nil && pos.Quantity > 0 {
			return true
		}
	}
	for _, pos := range strat.OptionPositions {
		if pos != nil && pos.Quantity > 0 {
			return true
		}
	}
	return false
}

func fetchStrategyDefaultParams(sc StrategyConfig) (map[string]interface{}, string, error) {
	name := effectiveOpenStrategy(sc)
	if name == "" {
		return map[string]interface{}{}, "", nil
	}
	args := []string{
		"--type", sc.Type,
		"--strategy", name,
	}
	stdout, stderr, err := runPythonReadOnly("shared_scripts/strategy_tuner_schema.py", args)
	if err != nil {
		return nil, "", fmt.Errorf("strategy_tuner_schema: %w (stderr: %s)", err, strings.TrimSpace(string(stderr)))
	}
	var resp struct {
		Error         string                 `json:"error,omitempty"`
		Description   string                 `json:"description"`
		DefaultParams map[string]interface{} `json:"default_params"`
	}
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return nil, "", fmt.Errorf("parse strategy schema: %w", err)
	}
	if resp.Error != "" {
		return nil, "", fmt.Errorf("%s", resp.Error)
	}
	if resp.DefaultParams == nil {
		resp.DefaultParams = map[string]interface{}{}
	}
	return resp.DefaultParams, resp.Description, nil
}

func buildUIStrategyConfig(sc StrategyConfig, defaults map[string]interface{}, _ string, hasOpen bool) UIStrategyConfigResponse {
	openName := effectiveOpenStrategy(sc)
	openParams := map[string]interface{}{}
	for k, v := range defaults {
		openParams[k] = v
	}
	for k, v := range sc.OpenStrategy.Params {
		openParams[k] = v
	}
	openRef := StrategyRef{Name: openName, Params: openParams}
	if sc.OpenStrategy.Name != "" {
		openRef.Name = sc.OpenStrategy.Name
	}

	fields := buildEditableFields(sc, openParams, defaults)
	restart := tunerApplyRequiresRestart(sc, hasOpen)

	return UIStrategyConfigResponse{
		StrategyID:           sc.ID,
		Type:                 sc.Type,
		Platform:             sc.Platform,
		Symbol:               strategyDisplaySymbol(sc),
		Timeframe:            strategyDisplayTimeframe(sc),
		OpenStrategy:         openRef,
		CloseStrategies:      append([]StrategyRef(nil), sc.CloseStrategies...),
		IntervalSeconds:      sc.IntervalSeconds,
		Direction:            EffectiveDirection(sc),
		InvertSignal:         sc.InvertSignal,
		Leverage:             EffectiveExchangeLeverage(sc),
		HTFFilter:            sc.HTFFilter,
		AllowedRegimes:       append([]string(nil), sc.AllowedRegimes...),
		StopLossPct:          sc.StopLossPct,
		StopLossATRMult:      sc.StopLossATRMult,
		DefaultParams:        defaults,
		EditableFields:       fields,
		HasOpenPosition:      hasOpen,
		ApplyRequiresRestart: restart,
	}
}

func buildEditableFields(sc StrategyConfig, mergedParams, defaults map[string]interface{}) []UIEditableField {
	fields := []UIEditableField{
		{Key: "interval_seconds", Label: "Interval (seconds)", Type: "number", Value: sc.IntervalSeconds, Group: "runtime"},
	}
	if sc.Type == "perps" || sc.Type == "manual" {
		fields = append(fields,
			UIEditableField{Key: "direction", Label: "Direction", Type: "select", Value: EffectiveDirection(sc), Options: []string{DirectionLong, DirectionShort, DirectionBoth}, Group: "runtime"},
			UIEditableField{Key: "invert_signal", Label: "Invert signal", Type: "boolean", Value: sc.InvertSignal, Group: "runtime"},
			UIEditableField{Key: "leverage", Label: "Leverage", Type: "number", Value: EffectiveExchangeLeverage(sc), Group: "runtime"},
		)
	}
	if sc.Type == "perps" || sc.Type == "manual" {
		fields = append(fields,
			UIEditableField{Key: "stop_loss_pct", Label: "Stop loss %", Type: "number", Value: ptrFloatValue(sc.StopLossPct), Group: "risk"},
			UIEditableField{Key: "stop_loss_atr_mult", Label: "Stop loss ATR mult", Type: "number", Value: ptrFloatValue(sc.StopLossATRMult), Group: "risk"},
		)
	}
	if sc.Type == "perps" {
		fields = append(fields,
			UIEditableField{Key: "htf_filter", Label: "HTF filter", Type: "boolean", Value: sc.HTFFilter, Group: "runtime"},
		)
	}

	keys := make([]string, 0, len(mergedParams))
	for k := range mergedParams {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		def := defaults[key]
		fields = append(fields, UIEditableField{
			Key:     "open_strategy.params." + key,
			Label:   humanizeParamKey(key),
			Type:    inferEditableFieldType(def),
			Value:   mergedParams[key],
			Default: def,
			Group:   "open_params",
		})
	}
	return fields
}

func humanizeParamKey(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(key, "_", " "), ".", " ")
}

func inferEditableFieldType(sample interface{}) string {
	switch sample.(type) {
	case bool:
		return "boolean"
	case float64, float32, int, int64, json.Number:
		return "number"
	default:
		return "text"
	}
}

func ptrFloatValue(v *float64) interface{} {
	if v == nil {
		return nil
	}
	return *v
}

func tunerApplyRequiresRestart(sc StrategyConfig, hasOpen bool) bool {
	if hasOpen {
		return true
	}
	// Indicator params live in open_strategy.params / args — hot reload rejects script/args shape changes.
	return true
}

func mergeStrategyTunerOverrides(base StrategyConfig, overrides map[string]json.RawMessage) (StrategyConfig, error) {
	out := base
	if len(overrides) == 0 {
		return out, nil
	}
	if raw, ok := overrides["interval_seconds"]; ok {
		var v int
		if err := json.Unmarshal(raw, &v); err != nil {
			return out, fmt.Errorf("interval_seconds: %w", err)
		}
		out.IntervalSeconds = v
	}
	if raw, ok := overrides["direction"]; ok {
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return out, fmt.Errorf("direction: %w", err)
		}
		out.Direction = strings.TrimSpace(v)
	}
	if raw, ok := overrides["invert_signal"]; ok {
		var v bool
		if err := json.Unmarshal(raw, &v); err != nil {
			return out, fmt.Errorf("invert_signal: %w", err)
		}
		out.InvertSignal = v
	}
	if raw, ok := overrides["leverage"]; ok {
		var v float64
		if err := json.Unmarshal(raw, &v); err != nil {
			return out, fmt.Errorf("leverage: %w", err)
		}
		out.Leverage = v
	}
	if raw, ok := overrides["htf_filter"]; ok {
		var v bool
		if err := json.Unmarshal(raw, &v); err != nil {
			return out, fmt.Errorf("htf_filter: %w", err)
		}
		out.HTFFilter = v
	}
	if raw, ok := overrides["stop_loss_pct"]; ok {
		v, err := decodeOptionalFloat(raw)
		if err != nil {
			return out, fmt.Errorf("stop_loss_pct: %w", err)
		}
		out.StopLossPct = v
	}
	if raw, ok := overrides["stop_loss_atr_mult"]; ok {
		v, err := decodeOptionalFloat(raw)
		if err != nil {
			return out, fmt.Errorf("stop_loss_atr_mult: %w", err)
		}
		out.StopLossATRMult = v
	}
	if raw, ok := overrides["open_strategy"]; ok {
		var ref StrategyRef
		if err := json.Unmarshal(raw, &ref); err != nil {
			return out, fmt.Errorf("open_strategy: %w", err)
		}
		if ref.Name != "" {
			out.OpenStrategy.Name = ref.Name
		}
		if ref.Params != nil {
			if out.OpenStrategy.Params == nil {
				out.OpenStrategy.Params = map[string]interface{}{}
			}
			for k, v := range ref.Params {
				out.OpenStrategy.Params[k] = v
			}
		}
	}
	for key, raw := range overrides {
		if !strings.HasPrefix(key, "open_strategy.params.") {
			continue
		}
		paramKey := strings.TrimPrefix(key, "open_strategy.params.")
		if paramKey == "" {
			continue
		}
		if out.OpenStrategy.Params == nil {
			out.OpenStrategy.Params = map[string]interface{}{}
		}
		var v interface{}
		if err := json.Unmarshal(raw, &v); err != nil {
			return out, fmt.Errorf("%s: %w", key, err)
		}
		out.OpenStrategy.Params[paramKey] = v
	}
	return out, nil
}

func decodeOptionalFloat(raw json.RawMessage) (*float64, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func simulateConfigPayload(sc StrategyConfig, regime *RegimeConfig) map[string]interface{} {
	payload := map[string]interface{}{
		"type":               sc.Type,
		"platform":           sc.Platform,
		"symbol":             strategyDisplaySymbol(sc),
		"timeframe":          strategyDisplayTimeframe(sc),
		"strategy":           effectiveOpenStrategy(sc),
		"open_strategy":      sc.OpenStrategy,
		"close_strategies":   sc.CloseStrategies,
		"htf_filter":         sc.HTFFilter,
		"allowed_regimes":    sc.AllowedRegimes,
		"initial_capital":    sc.InitialCapital,
		"stop_loss_pct":      sc.StopLossPct,
		"stop_loss_atr_mult": sc.StopLossATRMult,
	}
	if sc.StopLossMarginPct != nil {
		payload["stop_loss_margin_pct"] = *sc.StopLossMarginPct
	}
	if sc.TrailingStopPct != nil {
		payload["trailing_stop_pct"] = *sc.TrailingStopPct
	}
	if sc.TrailingStopATRMult != nil {
		payload["trailing_stop_atr_mult"] = *sc.TrailingStopATRMult
	}
	if sc.StopLossATRRegime != nil {
		payload["stop_loss_atr_regime"] = sc.StopLossATRRegime
	}
	if sc.TrailingStopATRRegime != nil {
		payload["trailing_stop_atr_regime"] = sc.TrailingStopATRRegime
	}
	if regime != nil {
		payload["regime"] = map[string]interface{}{
			"enabled":       regime.Enabled,
			"period":        regimePeriod(regime),
			"adx_threshold": regimeADXThreshold(regime),
		}
	}
	if sc.OpenStrategy.Name == "" && effectiveOpenStrategy(sc) != "" {
		open := sc.OpenStrategy
		open.Name = effectiveOpenStrategy(sc)
		payload["open_strategy"] = open
	}
	return payload
}

func regimePeriod(regime *RegimeConfig) int {
	if regime == nil {
		return 14
	}
	if regime.Period > 0 {
		return regime.Period
	}
	return 14
}

func regimeADXThreshold(regime *RegimeConfig) float64 {
	if regime == nil {
		return 20
	}
	if regime.ADXThreshold > 0 {
		return regime.ADXThreshold
	}
	return 20
}

func runStrategySimulate(candles []UICandle, configs map[string]map[string]interface{}) (map[string][]UITradeMarker, error) {
	type cfgItem struct {
		Label  string                 `json:"label"`
		Config map[string]interface{} `json:"config"`
	}
	items := make([]cfgItem, 0, len(configs))
	labels := make([]string, 0, len(configs))
	for label := range configs {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		items = append(items, cfgItem{Label: label, Config: configs[label]})
	}
	payload := map[string]interface{}{
		"candles": candles,
		"configs": items,
	}
	stdin, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	stdout, stderr, err := runPythonReadOnlyWithStdin("shared_scripts/simulate_strategy.py", nil, stdin)
	if err != nil {
		return nil, fmt.Errorf("simulate_strategy: %w (stderr: %s)", err, strings.TrimSpace(string(stderr)))
	}
	var resp struct {
		Error   string                     `json:"error,omitempty"`
		Markers map[string][]UITradeMarker `json:"markers"`
	}
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return nil, fmt.Errorf("parse simulate response: %w", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("%s", resp.Error)
	}
	if resp.Markers == nil {
		resp.Markers = map[string][]UITradeMarker{}
	}
	return resp.Markers, nil
}

func applyStrategyConfigPatch(configPath, strategyID string, merged StrategyConfig) (restartRequired bool, err error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return false, err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return false, fmt.Errorf("parse config: %w", err)
	}
	rawStrategies, ok := root["strategies"]
	if !ok {
		return false, fmt.Errorf("config has no strategies array")
	}
	var strategies []json.RawMessage
	if err := json.Unmarshal(rawStrategies, &strategies); err != nil {
		return false, fmt.Errorf("parse strategies: %w", err)
	}
	found := false
	for i, raw := range strategies {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		idRaw, ok := item["id"]
		if !ok {
			continue
		}
		var id string
		if err := json.Unmarshal(idRaw, &id); err != nil || id != strategyID {
			continue
		}
		patched, restart, patchErr := patchStrategyJSON(item, merged)
		if patchErr != nil {
			return false, patchErr
		}
		restartRequired = restart
		newRaw, err := json.Marshal(patched)
		if err != nil {
			return false, err
		}
		strategies[i] = newRaw
		found = true
		break
	}
	if !found {
		return false, fmt.Errorf("strategy %q not found in config", strategyID)
	}
	newStrategies, err := json.Marshal(strategies)
	if err != nil {
		return false, err
	}
	root["strategies"] = newStrategies
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false, err
	}
	out = append(out, '\n')
	tmpPath := configPath + ".tmp"
	if err := os.WriteFile(tmpPath, out, 0o600); err != nil {
		return false, err
	}
	if err := os.Rename(tmpPath, configPath); err != nil {
		return false, err
	}
	return restartRequired, nil
}

func patchStrategyJSON(item map[string]json.RawMessage, merged StrategyConfig) (map[string]json.RawMessage, bool, error) {
	restartRequired := true
	set := func(key string, value interface{}) error {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		item[key] = raw
		return nil
	}
	if merged.IntervalSeconds > 0 {
		if err := set("interval_seconds", merged.IntervalSeconds); err != nil {
			return item, restartRequired, err
		}
	}
	if merged.Direction != "" {
		if err := set("direction", merged.Direction); err != nil {
			return item, restartRequired, err
		}
	}
	if err := set("invert_signal", merged.InvertSignal); err != nil {
		return item, restartRequired, err
	}
	if merged.Leverage > 0 {
		if err := set("leverage", merged.Leverage); err != nil {
			return item, restartRequired, err
		}
	}
	if merged.Type == "perps" {
		if err := set("htf_filter", merged.HTFFilter); err != nil {
			return item, restartRequired, err
		}
	}
	if merged.StopLossPct != nil {
		if err := set("stop_loss_pct", merged.StopLossPct); err != nil {
			return item, restartRequired, err
		}
	}
	if merged.StopLossATRMult != nil {
		if err := set("stop_loss_atr_mult", merged.StopLossATRMult); err != nil {
			return item, restartRequired, err
		}
	}
	openName := effectiveOpenStrategy(merged)
	openRef := merged.OpenStrategy
	if openName != "" {
		openRef.Name = openName
	}
	if openRef.Name != "" || len(openRef.Params) > 0 {
		if err := set("open_strategy", openRef); err != nil {
			return item, restartRequired, err
		}
	}
	return item, restartRequired, nil
}
