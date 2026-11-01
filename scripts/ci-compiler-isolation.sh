#!/bin/sh
set -eu

: "${N01_GO_IMAGE:?the reviewed immutable Go image is required}"
: "${GITHUB_WORKSPACE:?the checked-out source is required}"
: "${GITHUB_SHA:?the exact workflow source revision is required}"
: "${RUNNER_TEMP:?owned CI scratch is required}"

case "$N01_GO_IMAGE" in
  docker.io/library/golang@sha256:ebd54034f076819b3054f155db53660ded951612bb4dfd277f933e62059e5d5a) ;;
  *) exit 1 ;;
esac

# These paths belong only to this new CI job. Refuse an existing directory;
# do not adopt or remove another job's cache, output or canaries.
mkdir "$RUNNER_TEMP/n01-compiler-build" "$RUNNER_TEMP/n01-compiler-output" "$RUNNER_TEMP/n01-host-canaries"
printf 'synthetic host-only engine-token canary\n' > "$RUNNER_TEMP/n01-host-canaries/engine-token"
printf 'synthetic host-only node-state canary\n' > "$RUNNER_TEMP/n01-host-canaries/node-state"
n01_secret_before="$(sha256sum "$RUNNER_TEMP/n01-host-canaries/engine-token")"
n01_state_before="$(sha256sum "$RUNNER_TEMP/n01-host-canaries/node-state")"

# Pull one reviewed image, then reuse it for the build and runtime. Build
# scratch is job-owned host storage; runtime scratch below has a hard tmpfs
# ceiling. The build's network access is not the runtime isolation result.
docker pull "$N01_GO_IMAGE"
docker image inspect "$N01_GO_IMAGE" --format '{{.Id}} {{.Os}}/{{.Architecture}}'
# Build with the host scratch owner's unprivileged UID/GID. With all
# capabilities dropped, container root cannot write another user's bind mount.
n01_build_uid="$(id -u)"
n01_build_gid="$(id -g)"
case "$n01_build_uid:$n01_build_gid" in *[!0-9:]*|0:*|*:0) exit 1 ;; esac
docker run --rm --read-only --user "$n01_build_uid:$n01_build_gid" \
  --cap-drop ALL --security-opt no-new-privileges \
  --memory 4g --memory-swap 4g --cpus 2 --pids-limit 128 \
  --mount "type=bind,src=$GITHUB_WORKSPACE,dst=/src,readonly" \
  --mount "type=bind,src=$RUNNER_TEMP/n01-compiler-build,dst=/scratch" \
  --mount "type=bind,src=$RUNNER_TEMP/n01-compiler-output,dst=/out" \
  --workdir /src \
  --env GOMAXPROCS=2 --env GOMEMLIMIT=512MiB --env GOTOOLCHAIN=local \
  --env GOCACHE=/scratch/gocache --env GOMODCACHE=/scratch/modules --env GOTMPDIR=/scratch/tmp --env TMPDIR=/scratch/tmp \
  --env GIT_CONFIG_COUNT=2 --env GIT_CONFIG_KEY_0=safe.directory --env GIT_CONFIG_VALUE_0=/src \
  --env GIT_CONFIG_KEY_1=safe.directory --env GIT_CONFIG_VALUE_1=/src/zvm-core \
  --env "N01_EXPECTED_SOURCE=$GITHUB_SHA" \
  "$N01_GO_IMAGE" sh -ceu '
    printf "N01_BUILD_STAGE=writable_scratch\n"
    test -w /scratch
    test -w /out
    mkdir -p /scratch/tmp
    test "$(go version)" = "go version go1.25.0 linux/amd64"
    go version
    printf "N01_BUILD_STAGE=source_identity\n"
    test "$(git rev-parse HEAD)" = "$N01_EXPECTED_SOURCE"
    test -z "$(git status --porcelain)"
    test "$(git -C zvm-core rev-parse HEAD)" = 29e6d9b30be94de3772c850c7a1e9811585a8833
    printf "N01_BUILD_STAGE=JSON_converter\n"
    go build -mod=readonly -o /out/test2json cmd/test2json
    test -x /out/test2json
    printf "N01_BUILD_STAGE=production_binary\n"
    go build -mod=readonly -race -o /out/zvm-node ./cmd/zvm-node
    printf "N01_BUILD_STAGE=profile_harness\n"
    go test -mod=readonly -race -c -o /out/solcd.test ./cmd/zvm-node
    go version -m /out/zvm-node
    n01_build_metadata="$(go version -m /out/zvm-node)"
    printf "%s\n" "$n01_build_metadata" | grep -F "vcs.revision=$N01_EXPECTED_SOURCE"
    printf "%s\n" "$n01_build_metadata" | grep -F "vcs.modified=false"
    chmod 0555 /out/zvm-node /out/solcd.test /out/test2json
    sha256sum /out/zvm-node /out/solcd.test /out/test2json
  '

test -f "$RUNNER_TEMP/n01-compiler-output/zvm-node"
test -f "$RUNNER_TEMP/n01-compiler-output/solcd.test"
test -f "$RUNNER_TEMP/n01-compiler-output/test2json"
printf 'N01_BUILD_REVISION=%s\n' "$GITHUB_SHA"
du -sk "$RUNNER_TEMP/n01-compiler-build" "$RUNNER_TEMP/n01-compiler-output"

# Only the three explicit binaries are mounted at runtime. Source, build cache,
# Docker socket and both host-only node canaries are outside its mount set.
printf 'N01_RUNTIME_BEGIN\n'
docker run --rm --read-only --user 10001:10001 --network none \
  --cap-drop ALL --security-opt no-new-privileges \
  --memory 512m --memory-swap 512m --cpus 2 --pids-limit 64 \
  --tmpfs /tmp:rw,nosuid,nodev,exec,size=64m,mode=1777 \
  --mount "type=bind,src=$RUNNER_TEMP/n01-compiler-output/zvm-node,dst=/qualified/zvm-node,readonly" \
  --mount "type=bind,src=$RUNNER_TEMP/n01-compiler-output/solcd.test,dst=/qualified/solcd.test,readonly" \
  --mount "type=bind,src=$RUNNER_TEMP/n01-compiler-output/test2json,dst=/qualified/test2json,readonly" \
  --env GOMAXPROCS=2 --env GOMEMLIMIT=128MiB --env TMPDIR=/tmp \
  --env ZVM_N01_CONTAINER_PROFILE=1 --env ZVM_N01_SOLCD_BINARY=/qualified/zvm-node \
  --env "ZVM_N01_HOST_SECRET_PATH=$RUNNER_TEMP/n01-host-canaries/engine-token" \
  --env "ZVM_N01_HOST_STATE_PATH=$RUNNER_TEMP/n01-host-canaries/node-state" \
  "$N01_GO_IMAGE" /qualified/test2json -t -p github.com/Zenoglyphs-VM/zvm-node/cmd/zvm-node \
  /qualified/solcd.test -test.v=test2json -test.count=1 -test.timeout=90s -test.run '^TestCompilerServiceContainerProfile$'
printf 'N01_RUNTIME_END\n'

test "$(sha256sum "$RUNNER_TEMP/n01-host-canaries/engine-token")" = "$n01_secret_before"
test "$(sha256sum "$RUNNER_TEMP/n01-host-canaries/node-state")" = "$n01_state_before"
printf 'N01_HOST_CANARIES_PRESERVED=true\n'
