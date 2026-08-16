# syntax=docker/dockerfile:1.10
ARG GO_VERSION=1.25.3

FROM golang:${GO_VERSION}-bookworm AS backend-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" -o /out/user ./app/user/cmd/user && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" -o /out/admin-init ./app/user/cmd/admin-init && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" -o /out/core-data ./app/core_data/cmd/core_data && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" -o /out/agent ./app/agent/cmd/agent

FROM golang:1.23-bookworm AS gateway-builder
WORKDIR /src/app/gateway
COPY app/gateway/go.mod app/gateway/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY app/gateway/ .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" -o /out/gateway ./cmd/gateway

FROM debian:bookworm-slim AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates gettext-base tzdata wget \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 goalgo \
    && useradd --uid 10001 --gid goalgo --no-create-home --home-dir /app --shell /usr/sbin/nologin goalgo \
    && install -d /run/goalgo \
    && chown goalgo:goalgo /run/goalgo
WORKDIR /app
ENV TZ=Asia/Shanghai
COPY --chmod=755 deploy/docker/render-config.sh /app/render-config.sh
COPY --chmod=755 deploy/docker/gateway-entrypoint.sh /app/gateway-entrypoint.sh

FROM runtime AS gateway
COPY --from=gateway-builder --chown=goalgo:goalgo /out/gateway /app/gateway
USER goalgo
EXPOSE 8080
ENTRYPOINT ["/app/gateway-entrypoint.sh"]
CMD ["/app/gateway", "-addr", "0.0.0.0:8080", "-conf", "/etc/goalgo/config/gateway.yaml", "-discovery.dsn", "consul://consul:8500"]

FROM runtime AS user
COPY --from=backend-builder --chown=goalgo:goalgo /out/user /app/user
COPY --from=backend-builder --chown=goalgo:goalgo /out/admin-init /app/admin-init
USER goalgo
EXPOSE 8000 9000
ENTRYPOINT ["/app/render-config.sh", "/etc/goalgo/config/user.yaml"]
CMD ["/app/user"]

FROM runtime AS agent
COPY --from=backend-builder --chown=goalgo:goalgo /out/agent /app/agent
USER goalgo
EXPOSE 8002 9002
ENTRYPOINT ["/app/render-config.sh", "/etc/goalgo/config/agent.yaml"]
CMD ["/app/agent"]

FROM runtime AS core-data-runtime
RUN apt-get update && apt-get install -y --no-install-recommends curl gnupg zstd \
    && install -d /usr/share/postgresql-common/pgdg \
    && curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc \
    && echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt bookworm-pgdg main" > /etc/apt/sources.list.d/pgdg.list \
    && apt-get update && apt-get install -y --no-install-recommends postgresql-client-18 \
    && rm -rf /var/lib/apt/lists/*

FROM core-data-runtime AS core-data
COPY --from=backend-builder --chown=goalgo:goalgo /out/core-data /app/core-data
USER goalgo
EXPOSE 8001 9001
ENTRYPOINT ["/app/render-config.sh", "/etc/goalgo/config/core-data.yaml"]
CMD ["/app/core-data"]
