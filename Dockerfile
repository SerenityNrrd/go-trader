# syntax=docker/dockerfile:1.7

# ---------- build stage ----------
FROM golang:1.26.2-bookworm AS builder

WORKDIR /src
# Cache deps first
COPY scheduler/go.mod scheduler/go.sum ./scheduler/
RUN cd scheduler && go mod download

# Source + version
COPY . .
ARG VERSION=container
RUN cd scheduler && \
    go build -ldflags "-X main.Version=${VERSION}" -o /out/go-trader .

# ---------- runtime stage ----------
# Official uv image = Python + uv preinstalled, no network fetch for the installer.
FROM ghcr.io/astral-sh/uv:python3.12-bookworm AS runtime

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl tini && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Python deps (cached unless pyproject/lock change). uv is already on PATH.
COPY pyproject.toml uv.lock ./
RUN uv sync --frozen --no-dev

# App source + compiled Go binary
COPY --from=builder /out/go-trader /usr/local/bin/go-trader
COPY shared_scripts   ./shared_scripts
COPY shared_strategies ./shared_strategies
COPY shared_tools     ./shared_tools
COPY platforms        ./platforms
COPY backtest         ./backtest
COPY scheduler        ./scheduler
# scheduler/config.json is gitignored — provide a placeholder so the path exists.
# Real config is injected at /var/lib/go-trader/config.json (see CMD).
RUN ln -sf /var/lib/go-trader/config.json /app/scheduler/config.json

# Persistent state lives outside the app tree (#1056 layout).
ENV GO_TRADER_STATE_DIR=/var/lib/go-trader \
    UV_PROJECT_ENVIRONMENT=/app/.venv \
    PATH=/app/.venv/bin:/usr/local/bin:$PATH
VOLUME ["/var/lib/go-trader"]

EXPOSE 8099

ENTRYPOINT ["/usr/bin/tini", "--"]
# Default: run with out-of-tree config. Override args via docker-compose command.
CMD ["go-trader", "--config", "/var/lib/go-trader/config.json"]
