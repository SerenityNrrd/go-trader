#!/usr/bin/env python3
"""
Generic CCXT strategy check + execution script.

One script for every ccxt-supported exchange (binanceus, alpaca, coinbase,
kraken, apex, …). Replaces per-platform check_<name>.py for spot crypto.

Usage (signal check):
    python3 check_ccxt.py <strategy> <symbol> <timeframe> \
        --exchange=<ccxt-id> [--mode=paper|live] [options]

Usage (live order, called by Go as Phase 2):
    python3 check_ccxt.py --execute --exchange=<ccxt-id> \
        --symbol=BTC/USD --side=buy --size=0.01 [--mode=live]

Output JSON shapes match check_strategy.py (signal) and check_okx.py
(execute) so the Go RunSpotCheck and a new RunCCXTExecute parsers stay
simple.

Env vars per exchange (ccxt convention): <EXCHANGE>_API_KEY / <EXCHANGE>_API_SECRET.
For alpaca specifically, also accepts the shorter ALPACA_KEY/ALPACA_SECRET.
"""

import sys
import os
import json
import math
import traceback
from datetime import datetime, timezone

sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_strategies', 'open', 'spot'))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))

from atr import ensure_atr_indicator, latest_atr
from regime import latest_regime, parse_regime_windows_spec_json, prepare_check_regime


# ─────────────────────────────────────────────
# Arg helpers (mirror check_strategy.py)
# ─────────────────────────────────────────────

def _arg_value(flag, default=None):
    prefix = flag + "="
    for arg in sys.argv:
        if arg.startswith(prefix):
            return arg.split("=", 1)[1]
    if flag not in sys.argv:
        return default
    idx = sys.argv.index(flag)
    if idx + 1 >= len(sys.argv):
        return default
    return sys.argv[idx + 1]


def _arg_float(flag):
    raw = _arg_value(flag)
    if raw in (None, ""):
        return None
    try:
        return float(raw)
    except (TypeError, ValueError):
        return None


def _position_ctx(position_side):
    ctx = {}
    if position_side:
        ctx["side"] = position_side
    for flag, key in (
        ("--position-avg-cost", "avg_cost"),
        ("--position-qty", "current_quantity"),
        ("--position-initial-qty", "initial_quantity"),
        ("--position-entry-atr", "entry_atr"),
    ):
        value = _arg_float(flag)
        if value is not None:
            ctx[key] = value
    regime = (_arg_value("--position-regime", "") or "").strip()
    if regime:
        ctx["regime"] = regime
    return ctx


# ─────────────────────────────────────────────
# CCXT exchange factory
# ─────────────────────────────────────────────

# Map of ccxt-id → env var name roots. Most exchanges use UPPER(ccxt-id) but
# a few aliases make life easier for operators.
ENV_ROOT_OVERRIDES = {
    "alpaca": ("ALPACA", True),   # accept both ALPACA_API_KEY and ALPACA_KEY
}


def _resolve_creds(exchange_id):
    """Return (api_key, secret) for the exchange from env vars. Both empty in
    paper-data mode (most ccxt exchanges serve public OHLCV anonymously)."""
    root, allow_short = ENV_ROOT_OVERRIDES.get(exchange_id, (exchange_id.upper(), False))
    key = os.environ.get(f"{root}_API_KEY") or ""
    secret = os.environ.get(f"{root}_API_SECRET") or ""
    if allow_short:
        key = key or os.environ.get(f"{root}_KEY") or ""
        secret = secret or os.environ.get(f"{root}_SECRET") or ""
    return key, secret


def _make_exchange(exchange_id):
    """Construct a ccxt exchange instance for the given id."""
    import ccxt

    cls = getattr(ccxt, exchange_id, None)
    if cls is None:
        raise RuntimeError(f"ccxt has no exchange named {exchange_id!r}")
    key, secret = _resolve_creds(exchange_id)
    cfg = {"enableRateLimit": True}
    if key and secret:
        cfg["apiKey"] = key
        cfg["secret"] = secret
    # Alpaca paper trading endpoint switch.
    if exchange_id == "alpaca" and os.environ.get("ALPACA_PAPER", "1") != "0":
        cfg["sandbox"] = True
    return cls(cfg)


def _normalize_pair(symbol, exchange_id):
    """Pass through if already a pair; otherwise append /USD (Alpaca) or
    /USDT (everything else). Operators can pass explicit pairs in args."""
    if "/" in symbol:
        return symbol
    base = symbol.upper().rstrip("/").split("/")[0]
    return f"{base}/USD" if exchange_id == "alpaca" else f"{base}/USDT"


# ─────────────────────────────────────────────
# Execute mode
# ─────────────────────────────────────────────

def _extract_fee(result):
    """Pull a signed fee (negative=credit, positive=debit in ccxt) out of a
    ccxt create_order response. Returns 0.0 when absent."""
    try:
        fee = (result.get("fees") or [{}])[0]
        if not fee and result.get("fee"):
            fee = result["fee"]
        cost = float(fee.get("cost") or 0)
        if cost < 0:  # ccxt convention: negative cost is a maker rebate
            cost = 0
        return cost
    except Exception:
        return 0.0


def run_execute(exchange_id, symbol, side, size, mode):
    if mode != "live":
        print(json.dumps({"error": "--execute requires --mode=live"}))
        sys.exit(1)
    try:
        ex = _make_exchange(exchange_id)
        ex.load_markets()
        pair = _normalize_pair(symbol, exchange_id)
        order = ex.create_order(pair, "market", side, size)
        fill = {
            "avg_px": float(order.get("average") or 0),
            "total_sz": float(order.get("filled") or 0),
            "oid": str(order.get("id") or ""),
            "fee": _extract_fee(order),
        }
        print(json.dumps({
            "execution": {
                "action": side,
                "symbol": pair,
                "size": size,
                "fill": fill,
            },
            "platform": exchange_id,
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }))
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        print(json.dumps({
            "execution": None,
            "platform": exchange_id,
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": str(e),
        }))
        sys.exit(1)


# ─────────────────────────────────────────────
# Signal check mode
# ─────────────────────────────────────────────

def _fetch_ohlcv(exchange_id, symbol, timeframe, limit):
    import pandas as pd
    ex = _make_exchange(exchange_id)
    pair = _normalize_pair(symbol, exchange_id)
    raw = ex.fetch_ohlcv(pair, timeframe, limit=limit) or []
    if not raw:
        return pd.DataFrame(columns=["timestamp", "open", "high", "low", "close", "volume"])
    df = pd.DataFrame(raw, columns=["timestamp", "open", "high", "low", "close", "volume"])
    df["datetime"] = pd.to_datetime(df["timestamp"], unit="ms", utc=True)
    df = df.set_index("datetime")
    df.sort_index(inplace=True)
    return df


def main():
    if "--probe-only" in sys.argv:
        sys.exit(0)

    # Execute mode short-circuit (mirrors check_okx.py)
    if "--execute" in sys.argv:
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("--execute", action="store_true")
        parser.add_argument("--exchange", required=True)
        parser.add_argument("--symbol", required=True)
        parser.add_argument("--side", required=True, choices=["buy", "sell"])
        parser.add_argument("--size", type=float, required=True)
        parser.add_argument("--mode", default="live")
        args = parser.parse_args()
        run_execute(args.exchange, args.symbol, args.side, args.size, args.mode)
        return

    # Signal check
    exchange_id = (_arg_value("--exchange") or "").strip()
    if not exchange_id:
        print(json.dumps({"error": "--exchange=<ccxt-id> is required"}))
        sys.exit(1)

    htf_filter_enabled = "--htf-filter" in sys.argv
    regime_enabled = "--regime-enabled" in sys.argv
    regime_windows_spec = parse_regime_windows_spec_json(_arg_value("--regime-windows-spec-json"))
    ohlcv_limit = int(_arg_value("--ohlcv-limit") or 200)
    regime_atr_window = (_arg_value("--regime-atr-window") or "").strip()
    regime_payload_json = _arg_value("--regime-payload-json")
    open_strategy = _arg_value("--open-strategy")
    close_strategies_raw = _arg_value("--close-strategies")
    position_side = (_arg_value("--position-side", "") or "").lower()
    position_ctx = _position_ctx(position_side)
    strategy_params = None
    if "--params" in sys.argv:
        idx = sys.argv.index("--params")
        if idx + 1 < len(sys.argv):
            strategy_params = json.loads(sys.argv[idx + 1])
    close_params_by_name = None
    strategy_refs_raw = _arg_value("--strategy-refs")
    if strategy_refs_raw:
        from strategy_composition import parse_strategy_refs_arg
        refs = parse_strategy_refs_arg(strategy_refs_raw)
        if refs:
            open_strategy = refs["open_name"]
            close_strategies_raw = refs["close_csv"]
            strategy_params = refs["open_params"]
            close_params_by_name = refs["close_params_by_name"]
    open_close_enabled = bool(open_strategy or close_strategies_raw)

    # Strip flags from positional args
    filtered = []
    skip_next = False
    for a in sys.argv[1:]:
        if skip_next:
            skip_next = False
            continue
        if a in ("--params", "--open-strategy", "--close-strategies", "--strategy-refs",
                 "--position-side", "--position-avg-cost", "--position-qty",
                 "--position-initial-qty", "--position-entry-atr",
                 "--position-regime",
                 "--regime-windows-spec-json", "--ohlcv-limit",
                 "--regime-atr-window", "--regime-directional-window",
                 "--regime-payload-json",
                 "--exchange", "--mark-price"):
            skip_next = True
            continue
        if a.startswith("--"):
            continue
        filtered.append(a)
    positional_args = filtered

    if len(positional_args) < 3:
        print(json.dumps({
            "error": f"Usage: {sys.argv[0]} <strategy> <symbol> <timeframe> --exchange=<ccxt-id>"
        }))
        sys.exit(1)

    strategy_name = positional_args[0]
    symbol = positional_args[1]
    timeframe = positional_args[2]
    symbol_b = positional_args[3] if len(positional_args) >= 4 else None

    try:
        from strategies import apply_strategy, get_strategy, list_strategies
        from close_registry_loader import (
            evaluate as close_evaluate,
            get_strategy as get_close_strategy,
            list_strategies as list_close_strategies,
        )
        from strategy_composition import (
            evaluate_open_close,
            finalize_decision,
            normalize_signal,
            parse_close_strategies,
            reject_backtest_only_strategies,
            validate_close_strategy_names,
        )

        configured_names = [open_strategy or strategy_name]
        reject_backtest_only_strategies(configured_names, get_strategy)
        validate_close_strategy_names(
            parse_close_strategies(close_strategies_raw),
            get_strategy,
            get_close_strategy,
            list_strategies,
            list_close_strategies,
        )

        needs_pair = "pairs_spread" in configured_names
        if needs_pair and not symbol_b:
            print("Warning: pairs_spread requires a secondary symbol; degrading.", file=sys.stderr)

        print(f"Fetching {symbol} {timeframe} from {exchange_id}...", file=sys.stderr)
        df = _fetch_ohlcv(exchange_id, symbol, timeframe, ohlcv_limit)

        if needs_pair and symbol_b:
            print(f"Fetching secondary {symbol_b} {timeframe} from {exchange_id}...", file=sys.stderr)
            df_b = _fetch_ohlcv(exchange_id, symbol_b, timeframe, ohlcv_limit)
            if df_b.empty:
                print(json.dumps({
                    "strategy": strategy_name, "symbol": symbol, "timeframe": timeframe,
                    "signal": 0, "price": 0, "indicators": {},
                    "timestamp": datetime.now(timezone.utc).isoformat(),
                    "error": f"No data returned for secondary symbol {symbol_b}",
                }))
                sys.exit(1)
            df = df.join(df_b[["close"]].rename(columns={"close": "close_b"}), how="inner")

        if df.empty or len(df) < 30:
            print(json.dumps({
                "strategy": strategy_name, "symbol": symbol, "timeframe": timeframe,
                "signal": 0, "price": 0, "indicators": {}, "regime": None,
                "timestamp": datetime.now(timezone.utc).isoformat(),
                "error": f"Insufficient data: {len(df)} candles",
            }))
            return

        stdout_regime, live_regime, strategy_regime = prepare_check_regime(
            df,
            regime_enabled=regime_enabled,
            windows_spec=regime_windows_spec,
            atr_window=regime_atr_window,
            injected_payload_json=regime_payload_json,
        )
        strategy_params = (strategy_params or {})
        strategy_params["regime"] = strategy_regime

        decision = None
        if open_close_enabled:
            market_ctx = {"mark_price": float(df["close"].iloc[-1])}
            atr_now = latest_atr(df)
            if atr_now > 0:
                market_ctx["atr"] = atr_now
            if live_regime:
                market_ctx["regime"] = live_regime
            evaluation = evaluate_open_close(
                apply_strategy, get_strategy, df, strategy_name, open_strategy,
                parse_close_strategies(close_strategies_raw),
                position_side, strategy_params, position_ctx,
                close_evaluate=close_evaluate,
                market_ctx=market_ctx,
                close_params_by_name=close_params_by_name,
            )
            result_df = evaluation.open_result_df
            signal = evaluation.open_signal
        else:
            result_df = apply_strategy(strategy_name, df, strategy_params)
            signal = normalize_signal(result_df.iloc[-1].get("signal", 0))

        ensure_atr_indicator(result_df)
        last = result_df.iloc[-1]
        price = float(last["close"])

        htf_info = {}
        htf_strategy_name = open_strategy or strategy_name
        if htf_filter_enabled and htf_strategy_name != "delta_neutral_funding":
            from htf_filter import htf_trend_filter, apply_htf_filter
            htf_info = htf_trend_filter(symbol, timeframe, lambda s, t, l: _fetch_ohlcv(exchange_id, s, t, l))
            original = signal
            signal = apply_htf_filter(signal, htf_info.get("htf_trend", 0))
            if signal != original:
                print(f"HTF filter: {original} → {signal}", file=sys.stderr)

        if open_close_enabled:
            decision = finalize_decision(evaluation, position_side, signal)
            signal = decision["signal"]

        indicators = {}
        for col in result_df.columns:
            if col in ("open", "high", "low", "close", "close_b", "volume",
                       "timestamp", "signal", "position", "datetime"):
                continue
            val = last.get(col)
            if val is not None:
                try:
                    fval = float(val)
                    if math.isfinite(fval):
                        indicators[col] = round(fval, 6)
                except (ValueError, TypeError):
                    pass
        for k, v in (htf_info or {}).items():
            if isinstance(v, (int, float)):
                indicators[k] = v

        output = {
            "strategy": strategy_name,
            "symbol": symbol,
            "timeframe": timeframe,
            "signal": signal,
            "price": round(price, 2),
            "indicators": indicators,
            "regime": stdout_regime,
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }
        if decision:
            output.update(decision)
        print(json.dumps(output))

    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        print(json.dumps({
            "strategy": strategy_name, "symbol": symbol, "timeframe": timeframe,
            "signal": 0, "price": 0, "indicators": {}, "regime": None,
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": str(e),
        }))
        sys.exit(1)


if __name__ == "__main__":
    main()
