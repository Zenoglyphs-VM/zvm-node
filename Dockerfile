# syntax=docker/dockerfile:1.7
#
# zvm-node: derivation, the index and read API, and the sandboxed compiler
# (`zvm-node solcd`).
#
#   docker build -t zvm-node .
#
# Build from a checkout with its zvm-core submodule; the module's
# `replace ... => ./zvm-core` resolves inside the build context.

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY zvm-core/go.mod zvm-core/go.sum ./zvm-core/
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/zvm-node ./cmd/zvm-node

FROM alpine:3.22
# gcompat: contract verification runs solc releases fetched from
# soliditylang.org, and some of them (0.8.12 to 0.8.23 among others) are linked
# against glibc rather than static.
RUN apk add --no-cache ca-certificates wget gcompat
# solcd runs as nobody and keeps its compiler cache in /solc; a named volume
# mounted there inherits this ownership, so it can write.
RUN mkdir -p /solc && chown 65534:65534 /solc
COPY --from=build /out/zvm-node /usr/local/bin/zvm-node
ENTRYPOINT ["zvm-node"]
