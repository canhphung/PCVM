# syntax=docker/dockerfile:1.7
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
    -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/pcvm ./cmd/pcvm

FROM debian:bookworm-slim
ARG TARGETARCH
LABEL org.opencontainers.image.source="https://github.com/canhphung/PCVM" \
      org.opencontainers.image.description="PCVM launcher for Pterodactyl" \
      org.opencontainers.image.licenses="MIT"
RUN if [ "$TARGETARCH" = "amd64" ]; then dpkg --add-architecture i386; fi \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
       apache2 build-essential ca-certificates git libatomic1 libcurl4 libicu72 libpulse0 libsqlite3-0 libssl3 libunwind8 \
       nginx-light tini unzip xz-utils zstd \
    && if [ "$TARGETARCH" = "amd64" ]; then \
         apt-get install -y --no-install-recommends libc6:i386 lib32gcc-s1 lib32stdc++6; \
       fi \
    && a2enmod proxy proxy_http headers \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --home-dir /home/container --uid 1000 --shell /bin/bash container
COPY --from=build /out/pcvm /usr/local/bin/pcvm
COPY runtime-manifest.json /opt/pcvm/runtime-manifest.json
RUN chmod 0755 /usr/local/bin/pcvm \
    && chmod 0644 /opt/pcvm/runtime-manifest.json \
    && chown -R container:container /home/container
USER container
WORKDIR /home/container
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/usr/local/bin/pcvm", "run"]
