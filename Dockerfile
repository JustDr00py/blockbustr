# syntax=docker/dockerfile:1

# ---- admin UI (P4.3) ----
FROM docker.io/library/node:24-slim AS ui
WORKDIR /src/web/admin
COPY web/admin/package.json web/admin/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/admin ./
RUN npm run build

# ---- build ----
FROM docker.io/library/golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY --from=ui /src/internal/adminui/dist ./internal/adminui/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/blockbustr ./cmd/blockbustr

# ---- runtime ----
FROM docker.io/library/debian:trixie-slim
# intel-media-va-driver-non-free (QSV/VAAPI on Intel) lives in non-free,
# which trixie-slim doesn't enable by default. libmfx-gen1.2 is the oneVPL
# GPU runtime behind ffmpeg's libvpl dispatcher: without it every QSV
# session fails with MFX error -9 and only VAAPI is usable.
RUN sed -i 's/^Components: main$/Components: main non-free non-free-firmware/' \
        /etc/apt/sources.list.d/debian.sources \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        tini \
        ffmpeg \
        vainfo \
        intel-media-va-driver-non-free \
        libmfx-gen1.2 \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 1000 --user-group --groups video --home-dir /app blockbustr \
    && mkdir -p /config /cache \
    && chown blockbustr:blockbustr /config /cache

WORKDIR /app
COPY --from=build /out/blockbustr ./blockbustr

USER blockbustr
VOLUME ["/config", "/cache"]
EXPOSE 8096
ENV BLOCKBUSTR_CACHE_DIR=/cache

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/app/blockbustr", "-config", "/config/config.yaml", "-healthcheck"]

ENTRYPOINT ["tini", "--", "/app/blockbustr"]
CMD ["-config", "/config/config.yaml"]
