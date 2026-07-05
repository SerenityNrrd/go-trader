package main

import (
	"encoding/json"
	"fmt"
)
// CCXTFill holds fill details from a live ccxt order (check_ccxt.py --execute).
type CCXTFill struct {
	AvgPx   float64 `json:"avg_px"`
	TotalSz float64 `json:"total_sz"`
	OID     string  `json:"oid,omitempty"`
	Fee     float64 `json:"fee,omitempty"`
}

// CCXTExecution is the execution block from check_ccxt.py --execute output.
type CCXTExecution struct {
	Action string    `json:"action"`
	Symbol string    `json:"symbol"`
	Size   float64   `json:"size"`
	Fill   *CCXTFill `json:"fill,omitempty"`
}

// CCXTExecuteResult is the top-level JSON from check_ccxt.py --execute.
type CCXTExecuteResult struct {
	Execution *CCXTExecution `json:"execution"`
	Platform  string         `json:"platform"`
	Timestamp string         `json:"timestamp"`
	Error     string         `json:"error,omitempty"`
}

// RunCCXTExecute runs check_ccxt.py in execute mode (live orders).
func RunCCXTExecute(script, exchangeID, symbol, side string, size float64) (*CCXTExecuteResult, string, error) {
	args := []string{
		"--execute",
		fmt.Sprintf("--exchange=%s", exchangeID),
		fmt.Sprintf("--symbol=%s", symbol),
		fmt.Sprintf("--side=%s", side),
		fmt.Sprintf("--size=%g", size),
		"--mode=live",
	}
	stdout, stderr, err := runPythonSideEffect(script, args)
	stderrStr := string(stderr)
	if err != nil {
		var result CCXTExecuteResult
		if jsonErr := json.Unmarshal(stdout, &result); jsonErr == nil && result.Error != "" {
			return &result, stderrStr, nil
		}
		return nil, stderrStr, fmt.Errorf("execute error: %w (stderr: %s)", err, stderrStr)
	}

	var result CCXTExecuteResult
	if err := json.Unmarshal(stdout, &result); err != nil {
		return nil, stderrStr, fmt.Errorf("parse execute output: %w (stdout: %s)", err, string(stdout))
	}
	return &result, stderrStr, nil
}

// ccxtSpotPlatforms is the set of platforms that route through the generic
// check_ccxt.py path. Existing per-platform paths (okx, robinhood, etc.) are
// intentionally NOT in here — they keep their dedicated check scripts.
var ccxtSpotPlatforms = map[string]bool{
	"alpaca":   true,
	"binanceus": true, // lives on the generic path now when sc.Script == check_ccxt.py
	"coinbase": true,
	"kraken":   true,
	"apex":     true,
	"luno":     true,
}

// isCCXTSpotStrategy reports whether a spot strategy routes through the
// generic CCXT path. Decided by Script path (not Platform alone) so an
// operator can still use the legacy default-spot path on binanceus/luno if
// they explicitly point at check_strategy.py.
func isCCXTSpotStrategy(sc StrategyConfig) bool {
	return sc.Script == "shared_scripts/check_ccxt.py"
}

// ccxtExchangeID returns the ccxt exchange id for a strategy. Defaults to
// sc.Platform; the operator can override via a `--exchange=` flag in Args.
func ccxtExchangeID(sc StrategyConfig) string {
	for _, a := range sc.Args {
		if len(a) > len("--exchange=") && a[:len("--exchange=")] == "--exchange=" {
			return a[len("--exchange="):]
		}
	}
	return sc.Platform
}

// ccxtIsLive reports whether --mode=live appears in strategy args.
func ccxtIsLive(args []string) bool {
	return isLiveArgs(args)
}

// runCCXTExecuteOrder places a live spot market order via check_ccxt.py
// --execute. Phase 3 (no lock). Mirrors runOKXExecuteOrder's spot branch:
// buy opens from cash, sell closes posQty (or posQty*CloseFraction on
// partial). SpotOrderSkipReason is consulted before spawning so a no-op
// signal-side branch never leaves an orphan on-chain fill (#298).
func runCCXTExecuteOrder(sc StrategyConfig, result *SpotResult, price, cash, posQty float64, posSide string, notifier *MultiNotifier, logger *StrategyLogger) (*CCXTExecuteResult, bool) {
	skip := SpotOrderSkipReason(result.Signal, posSide)
	if skip != "" {
		logger.Info("Skipping live order for %s: %s", result.Symbol, skip)
		return nil, false
	}
	isBuy := result.Signal == 1
	var size float64
	if isBuy {
		if cash < 1 || price <= 0 {
			logger.Info("Insufficient cash ($%.2f) for live buy %s", cash, result.Symbol)
			return nil, false
		}
		size = cash / price
	} else {
		if posQty <= 0 {
			logger.Info("No position to close for %s", result.Symbol)
			return nil, false
		}
		size = posQty
		if result.CloseFraction > 0 && result.CloseFraction < 1 {
			size = posQty * result.CloseFraction
		}
	}
	side := "buy"
	if !isBuy {
		side = "sell"
	}
	exchangeID := ccxtExchangeID(sc)
	logger.Info("Placing live %s %s size=%.6f on %s", side, result.Symbol, size, exchangeID)

	execResult, stderr, err := RunCCXTExecute(sc.Script, exchangeID, result.Symbol, side, size)
	if stderr != "" {
		logger.Info("execute stderr: %s", stderr)
	}
	direction := directionOpen
	if side == "sell" {
		direction = directionClose
	}
	if err != nil {
		logger.Error("Live execute failed: %v", err)
		notifyLiveExecFailure(notifier, sc, direction, result.Symbol, err.Error())
		return nil, false
	}
	if execResult.Error != "" {
		logger.Error("Live execute returned error: %s", execResult.Error)
		notifyLiveExecFailure(notifier, sc, direction, result.Symbol, execResult.Error)
		return nil, false
	}
	clearLiveExecThrottle(sc, direction, result.Symbol)
	return execResult, true
}

// executeCCXTResult applies a CCXT result to state. Must be called under Lock.
// Mirrors executeOKXResult's spot branch — SpotResult JSON shape is reused so
// ExecuteSpotSignalWithFillFeeDeferredOpen threads the live fill through.
func executeCCXTResult(sc StrategyConfig, s *StrategyState, db *StateDB, result *SpotResult, execResult *CCXTExecuteResult, signalStr string, price float64, regime *RegimeConfig, logger *StrategyLogger) (int, string) {
	fillPrice := price
	var fillQty float64
	var fillFee float64
	var fillOID string
	if execResult != nil && execResult.Execution != nil && execResult.Execution.Fill != nil && execResult.Execution.Fill.AvgPx > 0 {
		fillPrice = execResult.Execution.Fill.AvgPx
		fillQty = execResult.Execution.Fill.TotalSz
		fillFee = execResult.Execution.Fill.Fee
		fillOID = execResult.Execution.Fill.OID
		logger.Info("Live fill at $%.2f qty=%.6f (mid was $%.2f)", fillPrice, fillQty, price)
	}

	exec, err := ExecuteSpotSignalWithFillFeeDeferredOpen(s, result.Signal, result.Symbol, fillPrice, fillQty, fillFee, fillOID, result.CloseFraction, logger)
	if err != nil {
		logger.Error("Trade execution failed: %v", err)
		return 0, ""
	}
	trades := exec.TradesExecuted
	stampEntryATRIfOpened(s, result.Symbol, result.Indicators)
	stampPositionRegimeIfOpened(s, result.Symbol, regimePayloadValue(result.Regime), sc, regime)
	stampDirectionCertifiedAtOpenIfOpened(s, result.Symbol, exec.OpenTrade != nil, sc, regime)
	if pos, ok := s.Positions[result.Symbol]; ok {
		recordPositionOpen(s, sc, exec.OpenTrade, pos)
	}
	queueLLMEntryAnalysisIfOpened(sc, s, result.Symbol, trades, exec.OpenTrade, result.Indicators)

	detail := ""
	if trades > 0 {
		prefix := ""
		if execResult != nil && execResult.Execution != nil {
			prefix = fmt.Sprintf("[%s:%s] ", ccxtExchangeID(sc), execResult.Execution.Action)
		}
		detail = fmt.Sprintf("%s%s %s @ $%.2f", prefix, signalStr, result.Symbol, fillPrice)
	}
	return trades, detail
}
