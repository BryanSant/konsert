# Multi-stage, multi-arch Containerfile for konsertd worker daemon (linux/amd64, linux/arm64)
# Usage:
#   podman build --platform linux/amd64,linux/arm64 -t ghcr.io/bryansant/konsertd:latest -f Containerfile .

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
    -o /out/konsertd ./cmd/konsertd

FROM alpine:3.21

RUN apk add --no-cache \
    bash \
    ca-certificates \
    curl \
    git \
    openssh-client \
    procps \
    ripgrep \
    tzdata \
    && adduser -D -u 1000 -s /bin/bash konsert \
    && mkdir -p /workspace /var/tmp/konsert-attachments \
    && chown -R konsert:konsert /workspace /var/tmp/konsert-attachments

COPY --from=builder /out/konsertd /usr/local/bin/konsertd

ENV KONSERT_ISOLATION=container
ENV HOME=/home/konsert
ENV TERM=xterm-256color

WORKDIR /workspace
USER konsert

ENTRYPOINT ["/usr/local/bin/konsertd"]
CMD ["run"]
