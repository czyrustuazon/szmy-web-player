# syntax=docker/dockerfile:1

# ---- source tree plus media-kit, downloaded at the version go.mod names (go.sum checks it) ----
# Its Go packages come from the module cache; its browser modules are copied to web/lib, which
# the binary embeds. A go.work or web/lib from the host is never used (.dockerignore).
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS src
ENV GOWORK=off
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download \
    && mkdir -p /media-kit \
    && cp -R "$(go list -m -f '{{.Dir}}' github.com/czyrustuazon/lib-szmy-media-kit)/js/." /media-kit/ \
    && chmod -R u+w /media-kit
COPY . .
RUN mkdir -p web/lib && cp -R /media-kit web/lib/media-kit

# ---- build the Go binary (pure Go, no cgo) ----
FROM src AS build
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /out/masterplayer ./cmd/masterplayer

# `docker build --target test .` runs vet, the Go tests and the coverage gate.
FROM src AS test
ARG COVER_MIN=100
RUN --mount=type=cache,target=/root/.cache/go-build \
    go vet ./... && COVER_MIN="${COVER_MIN}" sh scripts/coverage.sh

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
# 7z unpacks .7z uploads (.zip needs nothing). Debian's package name changed between releases
# (p7zip-full, then "7zip" providing only a 7zz binary), so try both and make sure `7z` exists.
# ffmpeg converts WMA, APE, WavPack, WMV, FLV and similar formats to FLAC for playback.
# ca-certificates lets the player verify Google's TLS certificate during sign-in.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates ffmpeg \
    && (apt-get install -y --no-install-recommends p7zip-full || apt-get install -y --no-install-recommends 7zip) \
    && (command -v 7z >/dev/null 2>&1 || ln -s "$(command -v 7zz)" /usr/local/bin/7z) \
    && rm -rf /var/lib/apt/lists/*
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
