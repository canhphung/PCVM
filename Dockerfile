# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
ARG PROFILE=full
ARG VERSION=dev

FROM --platform=$BUILDPLATFORM golang:1.26-bookworm@sha256:6c5605ab3a9a9fb3c4eafe5b3d63cdbf3881caf113262b67862547b54a9db599 AS build
ARG TARGETOS
ARG TARGETARCH
ARG PROFILE
ARG VERSION
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN case "$PROFILE" in minecraft|games|apps|vm|full) ;; *) echo "unsupported PCVM image profile: $PROFILE" >&2; exit 1 ;; esac
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
    -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.imageProfile=${PROFILE}" \
    -o /out/pcvm ./cmd/pcvm

FROM debian:bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241 AS common
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       ca-certificates tini \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --home-dir /home/container --uid 1000 --shell /bin/bash container

FROM common AS native-runtime
RUN apt-get update \
    && apt-get install -y --no-install-recommends libatomic1 libcurl4 libssl3 zlib1g \
    && rm -rf /var/lib/apt/lists/*

FROM native-runtime AS minecraft

FROM common AS apps
# Keep the native compiler/linker needed by node-gyp and generic app builds,
# but omit optional profiling, LTO and sanitizer payloads from this runtime
# image. They are not part of PCVM's production build-toolchain contract.
RUN apt-get update \
    && apt-get install -y --no-install-recommends apache2 g++ gcc git libatomic1 libgssapi-krb5-2 libicu72 libssl3 make nginx-light zlib1g \
    && a2enmod proxy proxy_http headers \
    && rm -f /usr/bin/*-gcov* /usr/bin/*-lto-dump-* /usr/lib/gcc/*/*/lto1 \
    && find /usr/lib/gcc /usr/lib/"$(dpkg-architecture -qDEB_HOST_MULTIARCH)" \
       \( -type f -o -type l \) \
       \( -name 'libasan*' -o -name 'libhwasan*' -o -name 'liblsan*' -o -name 'libtsan*' -o -name 'libubsan*' \) -delete \
    && rm -rf /var/lib/apt/lists/* /usr/share/doc/* /usr/share/man/* /usr/share/locale/*

FROM native-runtime AS games
ARG TARGETARCH
RUN test "$TARGETARCH" = "amd64" \
    && dpkg --add-architecture i386 \
    && apt-get update \
    && apt-get install -y --no-install-recommends libicu72 libncursesw6 libpulse0 libsqlite3-0 libunwind8 xz-utils \
       libc6:i386 libatomic1:i386 lib32gcc-s1 lib32stdc++6 \
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

FROM native-runtime AS full
ARG TARGETARCH
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       apache2 g++ gcc genisoimage git libgssapi-krb5-2 libicu72 libncursesw6 libpulse0 libsqlite3-0 libunwind8 make \
       nginx-light qemu-utils xz-utils \
    && a2enmod proxy proxy_http headers \
    && if [ "$TARGETARCH" = "amd64" ]; then \
         dpkg --add-architecture i386 \
         && apt-get update \
         && apt-get install -y --no-install-recommends \
            libc6:i386 libatomic1:i386 lib32gcc-s1 lib32stdc++6 ovmf qemu-system-x86; \
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
