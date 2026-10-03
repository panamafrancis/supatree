#!/usr/bin/env bash
set -euo pipefail

# A sandboxed supatree to look at and drive by hand, or from a headless
# terminal (ht): an isolated HOME holding fixture repos, a stack, and trees in
# a spread of states, with a fake `claude` so opening an agent costs nothing.
#
#   scripts/ux-env.sh setup [--trees N]   build supatree, seed the sandbox (wipes an old one)
#   scripts/ux-env.sh start               run `supatree start` in it (in this terminal)
#   eval "$(scripts/ux-env.sh env)"       point this shell at it, for ad hoc commands
#   scripts/ux-env.sh focus left|right    move zellij focus (the sidebar is left)
#   scripts/ux-env.sh keys <chars>...     type into the focused pane, one argument at a time
#   scripts/ux-env.sh shot [pane]         print a pane, ">> " marking highlighted rows
#   scripts/ux-env.sh rebuild             rebuild supatree and restart the sidebar on it
#   scripts/ux-env.sh stop                kill the sandbox's zellij session
#   scripts/ux-env.sh teardown            stop, and delete the sandbox
#
# Notes for driving it headless:
#   - zellij draws nothing until its client gets its first input; send any key.
#   - Text snapshots carry no colour, so the cursor is invisible in them; use
#     `shot`, which reads zellij's own ANSI dump.
#   - Send Escape on its own: Escape and a key in one burst read as Alt+key.
#   - Agent tabs run under nono, which cannot nest: from inside a nono sandbox
#     (another agent's, say) they fail to start. Everything else works.

UX_DIR=${UX_DIR:-${TMPDIR:-/tmp}/supatree-ux}
UX_DIR=${UX_DIR%/}
# Short, fixed path: a unix socket path is capped near 104 bytes, and a fixed
# one lets a second terminal find the session.
UX_SOCKETS=${UX_SOCKETS:-/tmp/stux.$(id -u)}
SESSION=st-ux
REPO=$(cd "$(dirname "$0")/.." && pwd)

env_lines() {
    cat <<EOF
export HOME="$UX_DIR/home"
export XDG_CONFIG_HOME="$UX_DIR/home/.config" XDG_STATE_HOME="$UX_DIR/home/.local/state" XDG_CACHE_HOME="$UX_DIR/home/.cache"
export ZELLIJ_SOCKET_DIR="$UX_SOCKETS"
export PATH="$UX_DIR/bin:\$PATH"
export TERM="\${TERM:-xterm-256color}"
unset ZELLIJ ZELLIJ_SESSION_NAME ZELLIJ_PANE_ID
EOF
}

enter_env() {
    [ -d "$UX_DIR/home" ] || { echo "no sandbox at $UX_DIR — run: $0 setup" >&2; exit 1; }
    eval "$(env_lines)"
}

zj() { zellij -s "$SESSION" action "$@"; }

build() {
    (cd "$REPO" && go build -o "$UX_DIR/bin/supatree" ./cmd/supatree)
}

# fake_claude stands in for the agent CLI: it answers the `mcp list` init asks,
# and otherwise says how it was launched and echoes what is typed at it.
fake_claude() {
    cat > "$UX_DIR/bin/claude" <<'EOF'
#!/bin/sh
if [ "${1:-}" = mcp ]; then echo "supatree: supatree mcp"; exit 0; fi
echo "[fake claude] agent=${SUPATREE_AGENT:-?} tree=${SUPATREE_NAME:-?} cwd=$(pwd)"
echo "[fake claude] args: $*"
while IFS= read -r line; do echo "> $line"; done
EOF
    chmod +x "$UX_DIR/bin/claude"
}

fixture_repo() {
    local dir="$HOME/src/$1"
    mkdir -p "$dir"
    git -C "$dir" init -q
    echo "# $1" > "$dir/README.md"
    git -C "$dir" add README.md
    git -C "$dir" commit -qm initial
}

# record_agent registers a named agent without launching it, the way
# start_agent does ahead of the watcher.
record_agent() {
    local tree=$1 agent=$2 file="$XDG_STATE_HOME/supatree/trees/$1/agents.yml"
    [ -f "$file" ] || echo "agents:" > "$file"
    cat >> "$file" <<EOF
    - name: $agent
      model: claude
      address: st-$tree-$agent
EOF
}

setup() {
    local trees=0
    while [ $# -gt 0 ]; do
        case $1 in
            --trees) trees=$2; shift 2 ;;
            *) echo "unknown flag: $1" >&2; exit 2 ;;
        esac
    done
    stop_session
    rm -rf "$UX_DIR"
    mkdir -p "$UX_DIR/bin" "$UX_DIR/home" "$UX_SOCKETS"
    build
    fake_claude
    eval "$(env_lines)"

    git config --global user.email ux@test.local
    git config --global user.name ux
    git config --global init.defaultBranch main

    supatree init >/dev/null
    for r in api web infra; do fixture_repo "$r"; done
    supatree scaffold demo --repos="api=$HOME/src/api,web=$HOME/src/web,infra=$HOME/src/infra" >/dev/null
    local stack="$HOME/supatree/stacks/demo"
    cat > "$stack/supatree.yml" <<EOF
members:
  api: $HOME/src/api
  web: $HOME/src/web
  infra: $HOME/src/infra
deps:
  web: [api]
  api: [infra]
EOF
    git -C "$stack" commit -qam "dependency edges"

    # berlin: fresh, untouched.
    supatree new --stack demo --name berlin >/dev/null
    # paris: work in progress — a commit on one member, uncommitted edits in
    # another, and a named agent beside main.
    supatree new --stack demo --name paris >/dev/null
    local paris="$HOME/supatree/trees/paris/repos"
    echo "change" >> "$paris/api/README.md"
    git -C "$paris/api" commit -qam "api: a change"
    echo "draft" > "$paris/web/draft.txt"
    record_agent paris reviewer
    # oslo: renamed off its city slug, the way a tree looks before its PRs.
    supatree new --stack demo --name oslo >/dev/null
    (cd "$HOME/supatree/trees/oslo" && supatree rename-branch add-rate-limits oslo >/dev/null)
    # Extra trees, for scrolling and long-list behaviour.
    local i
    for ((i = 1; i <= trees; i++)); do
        supatree new --stack demo --name "extra-$i" >/dev/null
    done

    # A second PM below the default one (outside zellij this only registers it).
    supatree pm new infra >/dev/null

    echo "sandbox ready: $UX_DIR"
    supatree status
    echo
    echo "next: $0 start    (or, from ht: run '$0 start', then send any key)"
}

stop_session() {
    if [ -d "$UX_DIR/home" ]; then
        (eval "$(env_lines)"; zellij kill-session "$SESSION" >/dev/null 2>&1 || true
            zellij delete-session "$SESSION" --force >/dev/null 2>&1 || true)
    fi
}

# shot prints a pane from zellij's ANSI dump, marking rows drawn with a
# background colour or reverse video — the cursor, which plain text loses.
shot() {
    local pane=${1:-terminal_0}
    zj dump-screen --ansi --pane-id "$pane" | python3 -c '
import re, sys
hl = re.compile(r"\x1b\[(?:[0-9;]*;)?(?:7|4[0-8]|10[0-7])(?:;[0-9;]*)?m")
for line in sys.stdin.read().splitlines():
    plain = re.sub(r"\x1b\[[0-9;?]*[a-zA-Z]", "", line).rstrip()
    print((">> " if hl.search(line) and plain.strip() else "   ") + plain)
'
}

cmd=${1:-}
[ $# -gt 0 ] && shift
case $cmd in
    setup) setup "$@" ;;
    env) env_lines ;;
    start) enter_env; exec supatree start ux ;;
    focus) enter_env; zj move-focus "${1:-left}" ;;
    keys)
        enter_env
        for k in "$@"; do
            case $k in
                Enter) zj write 13 ;;
                Escape) zj write 27 ;;
                *) zj write-chars "$k" ;;
            esac
            sleep 0.2
        done ;;
    shot) enter_env; shot "$@" ;;
    rebuild)
        enter_env; build
        # The sidebar pane reruns supatree when it exits; quit it to pick up
        # the new binary.
        zj move-focus left; zj write-chars q; sleep 0.2; zj write-chars y ;;
    stop) stop_session ;;
    teardown) stop_session; rm -rf "$UX_DIR" ;;
    *) sed -n '4,25p' "$0" | sed 's/^# \{0,1\}//'; [ -z "$cmd" ] || exit 2 ;;
esac
