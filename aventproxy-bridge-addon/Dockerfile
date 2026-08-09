ARG BUILD_FROM
# Build on the runner's own arch and cross-compile: the binary is static, so
# emulating the target just to run the compiler would cost minutes for nothing.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS builder

ARG TARGETARCH
WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# cache on every build.
COPY avent-webrtc-bridge/go.mod avent-webrtc-bridge/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY avent-webrtc-bridge/ .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /avent-webrtc-bridge .

FROM ${BUILD_FROM}

# The static binary needs nothing else; ca-certificates is for the HTTPS calls
# to Tuya and is not in a bare alpine image.
RUN apk add --no-cache ca-certificates

COPY --from=builder /avent-webrtc-bridge /usr/local/bin/avent-webrtc-bridge
COPY --chmod=0755 run.sh /

CMD [ "/run.sh" ]
