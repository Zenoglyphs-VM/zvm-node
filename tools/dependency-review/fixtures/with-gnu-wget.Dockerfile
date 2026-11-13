# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
#
# zvm-node: derivation, the index and read API, and the sandboxed compiler
# (`zvm-node solcd`).
#
#   docker build -t zvm-node .
#
# Build from a checkout with its zvm-core submodule; the module's
# `replace ... => ./zvm-core` resolves inside the build context.

FROM golang:1.26.11-alpine3.24@sha256:ae52b5bc6ed045a9ed26ab03dc19327a802a9cc9aa8b27ed57f5c6b60579b0bb AS build
ENV GOTOOLCHAIN=local GOWORK=off GOTELEMETRY=off GOFLAGS=-mod=readonly
WORKDIR /src
COPY go.mod go.sum ./
COPY zvm-core/go.mod zvm-core/go.sum ./zvm-core/
RUN --mount=type=cache,target=/go/pkg/mod \
    test "$(go env GOVERSION)" = go1.26.11 && \
    go mod download && \
    go mod verify
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/zvm-node ./cmd/zvm-node
ARG NOTICE_SOURCE_COMMIT
ARG NOTICE_CORE_COMMIT
RUN --network=none --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOPROXY=off GOSUMDB=off go run ./tools/noticebundle \
      --binary /out/zvm-node --output /out/notices \
      --source-commit "$NOTICE_SOURCE_COMMIT" --core-commit "$NOTICE_CORE_COMMIT"

FROM alpine:3.22.8@sha256:49340b346798875b5592e5d420241f2f9cfba5f47e62ebb1446a1b34734e898f
# gcompat: contract verification runs solc releases fetched from
# soliditylang.org, and some of them (0.8.12 to 0.8.23 among others) are linked
# against glibc rather than static.
RUN apk add --no-cache ca-certificates=20261020-r0 wget=1.25.0-r1 gcompat=1.1.0-r4
# solcd runs as nobody and keeps its compiler cache in /solc; a named volume
# mounted there inherits this ownership, so it can write.
RUN mkdir -p /solc && chown 65534:65534 /solc
COPY --from=build /out/zvm-node /usr/local/bin/zvm-node
COPY --from=build /out/notices /usr/share/doc/zvm-node/notices
ENTRYPOINT ["zvm-node"]
