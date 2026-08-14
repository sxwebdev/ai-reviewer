# ai-reviewer — team AI code review for GitLab merge requests.
#
# Two stages: a Go builder that produces a static binary, and an Alpine runtime
# that additionally carries the Claude Code CLI, because reviews are performed
# by shelling out to `claude` (internal/llm/claude_cli.go). No credentials are
# baked in at any layer — every secret arrives at runtime through the
# environment or Vault.
#
#   docker build -t ai-reviewer:local .
#   docker build --build-arg VERSION=1.4.0 --build-arg COMMIT=$(git rev-parse --short HEAD) .
#
# The build context is the repository root, and it needs nothing but the module:
# the runtime stage fetches everything else it installs.

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------
FROM golang:1.26.5-alpine AS build

# git is here for the module fetch: anything the Go proxy cannot serve is
# resolved over VCS, and the failure without it is a confusing one. (VCS
# stamping is not a factor — .dockerignore excludes .git, so `go build` sees a
# non-repository and skips it; the version comes from the ldflags below.)
RUN apk add --no-cache git

WORKDIR /src

# Dependency layer first: it changes far less often than the source, so an
# ordinary code edit reuses the cached module download.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# The version is injected, never derived: the container has no .git and
# `git describe` inside a build stage would silently produce "dev" forever.
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

# CGO_ENABLED=0 keeps the binary static — the runtime stage is musl-based and a
# glibc-linked binary would not start there. Nothing in the tree needs a C
# toolchain, so this costs nothing.
#
# -trimpath keeps build paths out of the binary; -s -w drops the symbol table
# and DWARF, which does not conflict with the -X assignments below: those set
# package-level string variables at link time, before stripping.
RUN CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w \
  -X github.com/sxwebdev/ai-reviewer/internal/version.Version=${VERSION} \
  -X github.com/sxwebdev/ai-reviewer/internal/version.Commit=${COMMIT} \
  -X github.com/sxwebdev/ai-reviewer/internal/version.Date=${DATE}" \
  -o /out/ai-reviewer ./cmd/ai-reviewer

# ---------------------------------------------------------------------------
# Runtime
# ---------------------------------------------------------------------------
FROM alpine:3.24

# Runtime packages, and why each one is here:
#   ca-certificates — TLS to GitLab, Slack and the Anthropic API
#   git             — internal/git mirror-clones and adds worktrees by exec'ing git
#   tzdata          — the digest fires on Europe/Moscow wall-clock times. The Go
#                     binary embeds tzdata via internal/scheduler, so this is for
#                     the claude subprocess and for anyone exec'ing into the pod.
#   bash, curl      — required by Claude Code on musl
#   libgcc, libstdc++ — Claude Code's native binary links against both
#   ripgrep         — Claude Code's bundled ripgrep is glibc-only; the system one
#                     is used instead via USE_BUILTIN_RIPGREP=0 below
RUN apk add --no-cache ca-certificates git tzdata bash curl libgcc libstdc++ ripgrep \
  && adduser -D -u 10001 app \
  && mkdir -p /work \
  && chown app:app /work /home/app

# Claude Code comes from Anthropic's own apk repository, and the build takes
# whatever version it currently serves — rebuilding the image is how you get a
# newer agent. The signing key is fetched here rather than committed: nothing
# about this project should require carrying somebody else's public key in
# version control. Both the key and the packages come over TLS from the same
# origin, so the signature check is an integrity check (a truncated or corrupted
# download fails) rather than an independent trust root — but it does mean `apk`
# never needs --allow-untrusted.
#
# The consequence of not pinning, stated plainly: two builds of the same commit
# can ship different agents. `claude --version` in the running container is what
# tells you which one you have, and DISABLE_AUTOUPDATER below still keeps it from
# changing underneath a running container.
#
# Alternatives, if downloads.claude.ai is unreachable from a build network:
# `curl -fsSL https://claude.ai/install.sh | bash`, or `npm i -g
# @anthropic-ai/claude-code` on Node 22+ (both install the same native binary,
# neither is signature-verified by the package manager).
RUN curl -fsSL -o /etc/apk/keys/claude-code.rsa.pub \
  https://downloads.claude.ai/keys/claude-code.rsa.pub \
  && echo "https://downloads.claude.ai/claude-code/apk/stable" >> /etc/apk/repositories \
  && apk update \
  && apk add --no-cache claude-code

# TZ only affects log timestamps and anything that reads the container's local
# zone; the digest schedule carries Europe/Moscow explicitly and does not depend
# on it.
ENV TZ=Europe/Moscow
ENV HOME=/home/app
# The two variables below are set on the *service* process, and reviews run in a
# claude subprocess whose environment is an allowlist (internal/llm/auth.go):
# both are named in claudeEnvNames, which is what carries them across. Removing
# either name from that list would leave these ENVs silently ineffective, so the
# two files have to move together.
#
# Claude Code ships a glibc-linked ripgrep that cannot run on musl; dropping it
# does not degrade gracefully, it kills the Grep tool.
ENV USE_BUILTIN_RIPGREP=0
# The image is immutable: a self-update would write into a read-only filesystem
# at best, and silently change the reviewing agent mid-life at worst. The version
# is whatever the build installed, and `claude --version` proves which — rebuild
# the image to move it, do not let a running container move itself.
ENV DISABLE_AUTOUPDATER=1

COPY --from=build /out/ai-reviewer /usr/local/bin/ai-reviewer

USER app

# /work is review.workdir: ephemeral mirrors and worktrees, swept by the cleanup
# job. Mount an emptyDir (or a tmpfs) over it — the image layer must not be the
# place a 2 GB mirror lands.
WORKDIR /work

# ENTRYPOINT only, deliberately no CMD. This image is the whole CLI — `start`,
# `scan`, `review`, `digest`, `doctor`, `migrations` — and which of them runs is
# the caller's decision, not the image's:
#
#   docker run --rm ai-reviewer:latest doctor
#   docker run --rm ai-reviewer:latest review group/repo!42 --publish
#
# A default of `start` would make `docker run <image>` boot a server for someone
# who meant to look around, and would put one command's name in a layer everyone
# else has to override. With no CMD, a bare run prints the help and exits 0.
# The long-running case names itself in docker-compose.yml (`command: ["start"]`).
ENTRYPOINT ["ai-reviewer"]
