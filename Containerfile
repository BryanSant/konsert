# Multi-stage, multi-arch Containerfile for syred worker daemon (linux/amd64, linux/arm64)
# Usage:
#   podman build --platform linux/amd64,linux/arm64 -t ghcr.io/bryansant/syred:latest -f Containerfile .

ARG GO_VERSION=1.27
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder

ENV GOTOOLCHAIN=auto
WORKDIR /src
RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/syred ./cmd/syred

FROM alpine:3.24

RUN apk add --no-cache \
    bash \
    ca-certificates \
    curl \
    git \
    openssh-client \
    procps \
    ripgrep \
    tzdata \
    && adduser -D -u 1000 -s /bin/bash syre \
    && mkdir -p /workspace /var/tmp/syre-attachments \
    && chown -R syre:syre /workspace /var/tmp/syre-attachments

COPY --from=builder /out/syred /usr/local/bin/syred

ENV SYRE_ISOLATION=container
ENV HOME=/home/syre
ENV TERM=xterm-256color

WORKDIR /workspace
USER syre

ENTRYPOINT ["/usr/local/bin/syred"]
CMD ["run"]
