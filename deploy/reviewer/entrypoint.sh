#!/bin/sh
# Entrypoint for the supatree reviewer image. UNTESTED — see docs/reviewer.md.
#
# 1. load the bot's SSH key into an ssh-agent (nono's profiles let agents and
#    git use the agent socket, not key files);
# 2. write config.yml from the environment, on first start only — after that
#    the file on the volume is authoritative;
# 3. supatree init (directories, nono profile, MCP server registration);
# 4. clone each stack in SUPATREE_STACKS that is not registered yet;
# 5. exec the headless watcher, which runs the reviewer.
set -eu

: "${REVIEWER_LOGIN:?set REVIEWER_LOGIN to the bot's GitHub login}"
: "${GH_TOKEN:?set GH_TOKEN to the bot's token}"
: "${SUPATREE_STACKS:?set SUPATREE_STACKS to the stack repository URL(s), space-separated}"
if [ -z "${ANTHROPIC_API_KEY:-}" ] && [ -z "${CLAUDE_CODE_OAUTH_TOKEN:-}" ]; then
    echo "set ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN" >&2
    exit 1
fi

export XDG_CONFIG_HOME="${XDG_CONFIG_HOME:-$HOME/.config}"
export XDG_STATE_HOME="${XDG_STATE_HOME:-$HOME/.local/state}"
export XDG_CACHE_HOME="${XDG_CACHE_HOME:-$HOME/.cache}"
export SUPATREE_HEADLESS=1
# nono installs the profiles supatree's extends on first use, and with no TTY
# only when told it may.
export NONO_AUTO_MIGRATE=1

# 1. SSH.
SSH_KEY="${SSH_KEY_FILE:-/secrets/ssh/id}"
if [ -f "$SSH_KEY" ]; then
    export SSH_AUTH_SOCK="$HOME/.ssh-agent.sock"
    rm -f "$SSH_AUTH_SOCK"
    eval "$(ssh-agent -a "$SSH_AUTH_SOCK")" >/dev/null
    # A mounted Secret is not ours and is group-readable; ssh-add wants a
    # private key file, so it gets a 0600 copy that is gone right after.
    tmpkey=$(mktemp)
    cat "$SSH_KEY" > "$tmpkey"
    chmod 600 "$tmpkey"
    ssh-add "$tmpkey" 2>/dev/null || { rm -f "$tmpkey"; echo "could not load the SSH key at $SSH_KEY" >&2; exit 1; }
    rm -f "$tmpkey"
else
    echo "no SSH key at $SSH_KEY — cloning git@ URLs will fail" >&2
fi

# 2. Config, first start only.
CONFIG="$XDG_CONFIG_HOME/supatree/config.yml"
if [ ! -f "$CONFIG" ]; then
    mkdir -p "$(dirname "$CONFIG")"
    # Quoted heredoc: nothing below is expanded by this shell; the two values
    # are substituted afterwards, and checked to be plain tokens first.
    case "$REVIEWER_LOGIN" in *[!A-Za-z0-9-]*) echo "REVIEWER_LOGIN is not a GitHub login" >&2; exit 1;; esac
    LLM="${REVIEWER_LLM_GROUPING:-false}"
    case "$LLM" in true|false) ;; *) echo "REVIEWER_LLM_GROUPING must be true or false" >&2; exit 1;; esac
    cat > "$CONFIG.tmp" <<'YAML'
version: 1
stacks: []
notify_command: ["sh", "-c", "printf 'notify: %s: %s\\n' \"$1\" \"$2\" >> /proc/1/fd/2", "notify", "{title}", "{text}"]
reviewer:
  enabled: true
  login: @LOGIN@
  llm_grouping: @LLM@
YAML
    sed -e "s/@LOGIN@/$REVIEWER_LOGIN/" -e "s/@LLM@/$LLM/" "$CONFIG.tmp" > "$CONFIG"
    rm -f "$CONFIG.tmp"
    echo "wrote $CONFIG"
fi

# 3. Directories, nono profile, MCP server. Idempotent.
supatree init

# 4. Stacks. `stack clone` registers the stack and clones every member into
# the repository cache; one already on the volume is skipped.
for url in $SUPATREE_STACKS; do
    name=$(basename "$url" .git)
    if [ -d "$HOME/supatree/stacks/$name" ]; then
        continue
    fi
    supatree stack clone "$url"
done

# 5. The watcher. exec, so tini (PID 1) signals it directly.
exec supatree watch --headless
