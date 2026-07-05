"""
Alpaca Exchange Adapter — crypto spot via ccxt.

Alpaca exposes crypto through the same REST surface ccxt wraps, so this is a
thin ccxt adapter shaped like the BinanceUS/OKX ones. Stock trading is NOT
supported here (that needs a separate "stock" strategy type — see AGENTS.md).

Paper vs. live is selected by Alpaca endpoint:
    ALPACA_API_KEY / ALPACA_API_SECRET set      → authenticated (paper or live)
    ALPACA_PAPER=1 (default)                     → paper-api.alpaca.markets
    ALPACA_PAPER=0                               → api.alpaca.markets (live money)

Market data (OHLCV, tickers) on Alpaca is gated behind API keys, so unlike
the BinanceUS adapter this one will not function without credentials.
"""

import os
import sys
import math
from typing import Tuple

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..', 'shared_tools'))


def _alpaca_ccxt_config() -> dict:
    """Build a ccxt.alpaca config from environment. Paper endpoint by default."""
    cfg = {"enableRateLimit": True}
    key = os.environ.get("ALPACA_API_KEY", "")
    secret = os.environ.get("ALPACA_API_SECRET", "")
    if key and secret:
        cfg["apiKey"] = key
        cfg["secret"] = secret
    paper = os.environ.get("ALPACA_PAPER", "1") != "0"
    # ccxt's alpaca adapter reads these from config.urls — set via sandbox flag
    # equivalent for paper trading.
    if paper:
        cfg["sandbox"] = True
    return cfg


def _make_exchange():
    """Construct a ccxt alpaca exchange with env-driven creds."""
    import ccxt
    return ccxt.alpaca(_alpaca_ccxt_config())


class AlpacaExchangeAdapter:
    """
    ExchangeAdapter for Alpaca crypto.

    Paper mode (default): uses paper-api.alpaca.markets via ccxt's sandbox.
    Live mode: set ALPACA_PAPER=0 with real-money API credentials.
    """

    def __init__(self):
        self._exchange = _make_exchange()
        self._markets_loaded = False
        self._is_live = (
            bool(os.environ.get("ALPACA_API_KEY"))
            and bool(os.environ.get("ALPACA_API_SECRET"))
            and os.environ.get("ALPACA_PAPER", "1") == "0"
        )

    @property
    def is_live(self) -> bool:
        return self._is_live

    @property
    def mode(self) -> str:
        return "live" if self._is_live else "paper"

    @property
    def name(self) -> str:
        return "alpaca"

    def _load_markets(self):
        if not self._markets_loaded:
            self._exchange.load_markets()
            self._markets_loaded = True

    @staticmethod
    def _normalize_coin(underlying: str) -> str:
        """Alpaca crypto pairs are BASE/USD (e.g. BTC/USD), not USDT."""
        u = (underlying or "").upper().strip()
        return u.rstrip("/").split("/")[0]

    def get_spot_price(self, underlying: str) -> float:
        """Fetch current spot price for a crypto underlying (e.g. 'BTC')."""
        coin = self._normalize_coin(underlying)
        for suffix in ("/USD", "/USDT", "/USDC"):
            try:
                ticker = self._exchange.fetch_ticker(coin + suffix)
                price = ticker.get("last") or ticker.get("close") or 0
                if price and price > 0:
                    return float(price)
            except Exception:
                continue
        return 0.0

    def get_ohlcv(self, symbol: str, interval: str = "1h", limit: int = 200) -> list:
        """
        Fetch OHLCV candles from Alpaca.

        symbol may be either a coin ('BTC') or a ccxt pair ('BTC/USD').
        Returns list of [timestamp_ms, open, high, low, close, volume].
        """
        pair = symbol if "/" in symbol else self._normalize_coin(symbol) + "/USD"
        try:
            return self._exchange.fetch_ohlcv(pair, interval, limit=limit) or []
        except Exception:
            return []

    def get_ohlcv_closes(self, symbol: str, interval: str = "1h", limit: int = 200) -> list:
        candles = self.get_ohlcv(symbol, interval, limit)
        return [c[4] for c in candles] if candles else []

    def get_vol_metrics(self, underlying: str) -> Tuple[float, float]:
        """Compute 14-day historical vol and IV rank from daily OHLCV."""
        try:
            coin = self._normalize_coin(underlying)
            ohlcv = self._exchange.fetch_ohlcv(coin + "/USD", "1d", limit=90)
            if not ohlcv or len(ohlcv) < 15:
                return 0.60, 50.0
            closes = [c[4] for c in ohlcv]
            returns = [math.log(closes[i] / closes[i - 1]) for i in range(1, len(closes))]
            if len(returns) < 14:
                return 0.60, 50.0
            w = 14
            mean = sum(returns[-w:]) / w
            variance = sum((r - mean) ** 2 for r in returns[-w:]) / w
            vol = math.sqrt(variance) * math.sqrt(365)

            hvs = []
            for i in range(len(returns) - w + 1):
                chunk = returns[i:i + w]
                m = sum(chunk) / w
                v = sum((r - m) ** 2 for r in chunk) / w
                hvs.append(math.sqrt(v) * math.sqrt(365) * 100)
            current_hv = vol * 100
            hv_min, hv_max = min(hvs), max(hvs)
            if hv_max > hv_min:
                iv_rank = (current_hv - hv_min) / (hv_max - hv_min) * 100
                iv_rank = round(min(max(iv_rank, 0.0), 100.0), 1)
            else:
                iv_rank = 50.0
            return round(vol, 4), iv_rank
        except Exception:
            return 0.60, 50.0

    # Options not supported on Alpaca crypto.
    def get_real_expiry(self, underlying: str, target_dte: int) -> Tuple[str, int]:
        raise NotImplementedError("Alpaca does not support options")

    def get_real_strike(self, underlying: str, expiry: str,
                        option_type: str, target_strike: float) -> float:
        raise NotImplementedError("Alpaca does not support options")

    def get_premium_and_greeks(self, underlying: str, option_type: str,
                               strike: float, expiry: str, dte: float,
                               spot: float, vol: float) -> Tuple[float, float, dict]:
        raise NotImplementedError("Alpaca does not support options")
