"""
RSI Momentum — trend-following RSI strategy using 50-line as momentum divider.

Based on the 2026 YouTube strategy that returned 6,047% over 6 years on 4H.

Core idea: RSI 50 is the dividing line, NOT 70/30. Buy when RSI confirms
momentum above 50, not when it's oversold.

Signal A (Momentum Cross): RSI crosses above its 9-period EMA while RSI_EMA > 50
Signal B (50-Level Retest): RSI was > 56, pulls back to 44-55, RSI_EMA still > 50
Filter: close > 200 EMA (stay on right side of macro trend)
"""

import numpy as np
import pandas as pd


def _rsi(close: np.ndarray, period: int = 14) -> np.ndarray:
    """Compute RSI using Wilder's smoothing. Returns 50.0 during warmup (neutral)."""
    n = len(close)
    rsi = np.full(n, 50.0)  # neutral during warmup
    if n < period + 1:
        return rsi

    delta = np.diff(close)
    gain = np.where(delta > 0, delta, 0.0)
    loss = np.where(delta < 0, -delta, 0.0)

    avg_gain = np.mean(gain[:period])
    avg_loss = np.mean(loss[:period])

    if avg_loss == 0:
        rsi[period] = 100.0
    else:
        rs = avg_gain / avg_loss
        rsi[period] = 100.0 - (100.0 / (1.0 + rs))

    for i in range(period + 1, n):
        avg_gain = (avg_gain * (period - 1) + gain[i - 1]) / period
        avg_loss = (avg_loss * (period - 1) + loss[i - 1]) / period
        if avg_loss == 0:
            rsi[i] = 100.0
        else:
            rs = avg_gain / avg_loss
            rsi[i] = 100.0 - (100.0 / (1.0 + rs))

    return rsi


def _ema(series: np.ndarray, period: int) -> np.ndarray:
    """Compute EMA. Seeds from first non-NaN value, fills forward."""
    n = len(series)
    result = np.full(n, np.nan)
    if n < period:
        return result

    alpha = 2.0 / (period + 1.0)
    
    # Find first valid (non-NaN) value to seed
    seed_idx = period - 1
    while seed_idx < n and np.isnan(series[seed_idx]):
        seed_idx += 1
    if seed_idx >= n:
        return result
    
    result[seed_idx] = series[seed_idx]
    for i in range(seed_idx + 1, n):
        if np.isnan(series[i]):
            result[i] = result[i - 1]
        else:
            result[i] = alpha * series[i] + (1 - alpha) * result[i - 1]
    return result


def rsi_momentum_core(
    df: pd.DataFrame,
    rsi_period: int = 14,
    rsi_ema_period: int = 9,
    trend_ema_period: int = 200,
    retest_high: float = 56.0,
    retest_low: float = 44.0,
    retest_upper: float = 55.0,
) -> pd.DataFrame:
    """
    RSI Momentum strategy — trend-following using RSI 50 as momentum divider.

    Parameters
    ----------
    df : DataFrame with open, high, low, close, volume columns
    rsi_period : RSI calculation period
    rsi_ema_period : EMA period applied to RSI values
    trend_ema_period : long-term EMA for macro trend filter
    retest_high : RSI must have been above this to qualify for retest
    retest_low : lower bound of retest zone
    retest_upper : upper bound of retest zone

    Returns
    -------
    DataFrame with added 'signal' column: 1 (buy), -1 (sell), 0 (hold)
    """
    result = df.copy()
    result["signal"] = 0

    n = len(result)
    min_bars = max(rsi_period, rsi_ema_period, trend_ema_period) + 5
    if n < min_bars:
        return result

    close = result["close"].values

    # Compute RSI
    rsi_vals = _rsi(close, rsi_period)

    # Compute RSI EMA
    rsi_ema_vals = _ema(rsi_vals, rsi_ema_period)

    # Compute trend EMA (200 EMA on price)
    trend_ema_vals = _ema(close, trend_ema_period)

    sig_col = result.columns.get_loc("signal")

    # Track whether RSI has been above retest_high recently
    rsi_was_high = False

    for i in range(min_bars, n):
        if np.isnan(rsi_vals[i]) or np.isnan(rsi_ema_vals[i]) or np.isnan(trend_ema_vals[i]):
            continue

        # Macro trend filter: price must be above 200 EMA
        if close[i] <= trend_ema_vals[i]:
            rsi_was_high = False
            continue

        # Track if RSI has been above retest_high
        if rsi_vals[i] > retest_high:
            rsi_was_high = True

        # Signal A: Momentum Cross — RSI crosses above RSI_EMA while RSI_EMA > 50
        if (
            rsi_ema_vals[i] > 50.0
            and rsi_vals[i] > rsi_ema_vals[i]
            and not np.isnan(rsi_vals[i - 1])
            and not np.isnan(rsi_ema_vals[i - 1])
            and rsi_vals[i - 1] <= rsi_ema_vals[i - 1]
        ):
            result.iloc[i, sig_col] = 1
            rsi_was_high = False
            continue

        # Signal B: 50-Level Retest — RSI was high, now in retest zone, RSI_EMA > 50
        if (
            rsi_was_high
            and rsi_ema_vals[i] > 50.0
            and retest_low <= rsi_vals[i] <= retest_upper
        ):
            result.iloc[i, sig_col] = 1
            rsi_was_high = False
            continue

    return result
