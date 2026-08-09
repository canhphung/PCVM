# syntax=docker/dockerfile:1.7
ARG PROFILE=full
ARG VERSION=dev

FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
ARG PROFILE
ARG VERSION
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN case "$PROFILE" in core|games|vm|full) ;; *) echo "unsupported PCVM image profile: $PROFILE" >&2; exit 1 ;; esac
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
    -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.imageProfile=${PROFILE}" \
    -o /out/pcvm ./cmd/pcvm

FROM debian:bookworm-slim AS common
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       apache2 build-essential ca-certificates git libatomic1 libcurl4 libssl3 zlib1g \
       nginx-light openssl tini unzip xz-utils zstd \
    && a2enmod proxy proxy_http headers \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --home-dir /home/container --uid 1000 --shell /bin/bash container

FROM common AS core

FROM common AS games
ARG TARGETARCH
RUN apt-get update \
    && apt-get install -y --no-install-recommends libicu72 libncursesw6 libpulse0 libsqlite3-0 libunwind8 \
    && if [ "$TARGETARCH" = "amd64" ]; then \
      dpkg --add-architecture i386 \
      && apt-get update \
      && apt-get install -y --no-install-recommends \
         libc6:i386 libatomic1:i386 lib32gcc-s1 lib32stdc++6 \
      ; fi \
    && rm -rf /var/lib/apt/lists/*

FROM common AS vm
ARG TARGETARCH
RUN apt-get update \
    && apt-get install -y --no-install-recommends genisoimage qemu-utils \
    && if [ "$TARGETARCH" = "amd64" ]; then \
         apt-get install -y --no-install-recommends ovmf qemu-system-x86; \
       else \
         apt-get install -y --no-install-recommends qemu-efi-aarch64 qemu-system-arm; \
       fi \
    && rm -rf /var/lib/apt/lists/*

FROM games AS full
ARG TARGETARCH
RUN apt-get update \
    && apt-get install -y --no-install-recommends genisoimage qemu-utils \
    && if [ "$TARGETARCH" = "amd64" ]; then \
         apt-get install -y --no-install-recommends ovmf qemu-system-x86; \
       else \
         apt-get install -y --no-install-recommends qemu-efi-aarch64 qemu-system-arm; \
       fi \
    && rm -rf /var/lib/apt/lists/*

FROM ${PROFILE} AS final
ARG PROFILE
ARG VERSION
LABEL org.opencontainers.image.source="https://github.com/canhphung/PCVM" \
      org.opencontainers.image.description="PCVM launcher for Pterodactyl (${PROFILE} profile)" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      io.pcvm.image.profile="${PROFILE}"
COPY --from=build /out/pcvm /usr/local/bin/pcvm
COPY runtime-manifest.json /opt/pcvm/runtime-manifest.json
RUN chmod 0755 /usr/local/bin/pcvm \
    && chmod 0644 /opt/pcvm/runtime-manifest.json \
    && chown -R container:container /home/container
USER container
WORKDIR /home/container
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/usr/local/bin/pcvm", "run"]
