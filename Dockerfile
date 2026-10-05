# syntax=docker/dockerfile:1

# ---- build the Go binary (pure Go, no cgo, no third-party modules) ----
FROM --platform=$BUILDPLATFORM golang:1.22-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /out/masterplayer ./cmd/masterplayer

# `docker build --target test .` runs vet, the Go tests and the coverage gate.
FROM golang:1.22-bookworm AS test
ARG COVER_MIN=85
WORKDIR /src
COPY . .
RUN go vet ./... && COVER_MIN="${COVER_MIN}" sh scripts/coverage.sh

# ---- vgmstream-cli: prebuilt static binary from the official release ----
# Upstream only publishes x86-64 Linux builds. On other architectures the image still
# works; BRSTM/BCSTM/BFSTM and other game formats are then unavailable.
FROM debian:bookworm-slim AS vgm
ARG TARGETARCH
ARG VGMSTREAM_VERSION=r2117
ARG VGMSTREAM_SHA256=2f98c77f756079f63fbd119939067f1ed461d77e70993bc4cc372736d859c84a
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl unzip \
    && rm -rf /var/lib/apt/lists/*
RUN mkdir -p /out \
    && if [ "${TARGETARCH:-amd64}" = "amd64" ]; then \
         curl -fsSL -o /tmp/vgm.zip "https://github.com/vgmstream/vgmstream/releases/download/${VGMSTREAM_VERSION}/vgmstream-linux.zip" \
         && echo "${VGMSTREAM_SHA256}  /tmp/vgm.zip" | sha256sum -c - \
         && unzip -q /tmp/vgm.zip -d /out \
         && chmod +x /out/vgmstream-cli; \
       else \
         echo "no prebuilt vgmstream for ${TARGETARCH}; game formats disabled"; \
       fi

# ---- runtime ----
FROM debian:bookworm-slim
COPY --from=build /out/masterplayer /usr/local/bin/masterplayer
COPY --from=vgm /out/ /usr/local/bin/
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

ENV MP_PORT=8080 \
    MP_MUSIC_DIR=/music \
    MP_DATA_DIR=/data \
    PUID=1000 \
    PGID=1000
VOLUME ["/music", "/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["masterplayer", "-healthcheck"]
ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["masterplayer"]
