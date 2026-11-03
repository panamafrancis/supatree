# syntax=docker/dockerfile:1
#
# The supatree reviewer: `supatree watch --headless` with the reviewer enabled.
# UNTESTED — see docs/reviewer.md, "What to verify first".
#
# Build from the repository root:
#   docker build -f deploy/reviewer/Dockerfile -t supatree-reviewer .
#
# The image carries only what supatree needs. The review agent also builds and
# tests the repositories it reviews, so add their toolchains (Go, Node, …) in a
# layer of your own — see the TOOLCHAINS section below.
#
# linux/amd64 only as written: the nono and gh downloads name the architecture.

ARG GO_VERSION=1.27.3

FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/supatree ./cmd/supatree

FROM node:22-bookworm-slim

# Pinned, checksummed downloads. Bump together with the versions CI tests
# (.github/workflows/ci.yml pins nono).
ARG NONO_VERSION=v0.80.0
ARG NONO_SHA256=6da665b61999570c10dffc7da78828fe1fb7f3cac53319e505cebecaef17037f
ARG GH_VERSION=2.62.0
ARG CLAUDE_CODE_VERSION=2.1.308

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl git openssh-client tini make \
 && rm -rf /var/lib/apt/lists/*

RUN set -eux; \
    tarball="nono-${NONO_VERSION}-x86_64-unknown-linux-gnu.tar.gz"; \
    curl -fsSLO "https://github.com/always-further/nono/releases/download/${NONO_VERSION}/${tarball}"; \
    echo "${NONO_SHA256}  ${tarball}" | sha256sum -c -; \
    tar xzf "${tarball}" -C /usr/local/bin nono; \
    rm "${tarball}"; \
    nono --version

# Verify against the checksums file gh publishes with each release.
RUN set -eux; \
    tarball="gh_${GH_VERSION}_linux_amd64.tar.gz"; \
    curl -fsSLO "https://github.com/cli/cli/releases/download/v${GH_VERSION}/${tarball}"; \
    curl -fsSL "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_checksums.txt" | grep " ${tarball}\$" | sha256sum -c -; \
    tar xzf "${tarball}"; \
    mv "gh_${GH_VERSION}_linux_amd64/bin/gh" /usr/local/bin/gh; \
    rm -rf "${tarball}" "gh_${GH_VERSION}_linux_amd64"; \
    gh --version

RUN npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" && claude --version

# GitHub's SSH host keys, from GitHub's published list rather than a scan at
# build time. Check them against
# https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/githubs-ssh-key-fingerprints
RUN printf '%s\n' \
    'github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl' \
    > /etc/ssh/ssh_known_hosts

# --- TOOLCHAINS -------------------------------------------------------------
# Add what your repositories build and test with, e.g.:
#   COPY --from=golang:1.27.3-bookworm /usr/local/go /usr/local/go
#   ENV PATH=/usr/local/go/bin:$PATH
# -----------------------------------------------------------------------------

COPY --from=build /out/supatree /usr/local/bin/supatree
COPY deploy/reviewer/entrypoint.sh /usr/local/bin/supatree-reviewer
RUN chmod 0755 /usr/local/bin/supatree-reviewer

# The bot's whole home is the persistent volume: config, state, repository
# cache, review trees, Claude settings.
RUN useradd --create-home --uid 10001 --shell /bin/sh reviewer
USER reviewer
ENV HOME=/home/reviewer
WORKDIR /home/reviewer

# tini is PID 1: it reaps the agents' orphaned children and passes SIGTERM on
# to the watcher, which stops running reviews and exits.
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/supatree-reviewer"]
