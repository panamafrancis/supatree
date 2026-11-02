# The automated reviewer

The reviewer is a GitHub account that supatree operates. You request a review from it the way you would from a colleague, and it:

1. notices the request,
2. works out which other pull requests belong to the same cross-repo change,
3. checks them all out side by side in one review tree,
4. has a headless Claude agent review them, building and testing each repo,
5. posts one batched review per pull request, with inline comments,
6. re-reviews when you click **Re-request review**, and **approves** once nothing it finds is blocking.

It runs inside `supatree watch`, on your machine or in a container. This page covers setting it up, running it, and deploying it.

- [How it works](#how-it-works)
- [Requirements](#requirements)
- [Setting up the bot account](#setting-up-the-bot-account)
- [Configuration](#configuration)
- [Review guidance](#review-guidance)
- [Running it locally](#running-it-locally)
- [Deploying it to a container or Kubernetes](#deploying-it-to-a-container-or-kubernetes)
- [Using it](#using-it)
- [Monitoring and operating it](#monitoring-and-operating-it)
- [Security](#security)
- [Limitations](#limitations)

## How it works

Every 30 seconds the watcher polls each watched repository's pull request list. The poll is conditional, so it costs nothing when nothing has changed. From each listing the watcher keeps every open pull request that names the bot as a requested reviewer. The reviewer then:

- **Groups.** It decides which waiting pull requests belong to one change. The evidence it uses, strongest first:
  1. The *change marker* that `create_prs` writes into every pull request of a supatree.
  2. A description that links another waiting pull request.
  3. The same author pushing the same branch name, or the same `st/<slug>/` prefix, to several repositories.
  4. Optionally, an LLM pass over one author's otherwise-unrelated pull requests. See `llm_grouping`.

  A group holds at most one pull request per repository, all from one stack. Where the evidence conflicts, the reviewer splits rather than merges.
- **Waits.** A new group waits `group_window` (5 minutes by default) after its newest request, so pull requests opened a minute apart land in the same tree.
- **Creates a review tree.** Every repository is checked out at its pull request's head. These trees belong to the reviewer, which never touches trees you made yourself.
- **Reviews.** It starts a headless agent with a fixed brief. The agent reads existing comments, builds, lints and tests every repository, checks the repositories against each other, and posts one review per waiting pull request:
  - **APPROVE** when nothing blocks;
  - **REQUEST_CHANGES** when something does;
  - **COMMENT** instead of approving when the pull request's checks are failing.
- **Confirms.** It asks GitHub what was actually posted. A run that posted nothing counts as a failed attempt. It is retried after 10, 20, 40… minutes, and after `max_attempts` the reviewer gives up and sends a notification.
- **Re-reviews.** GitHub removes a reviewer from the request list when their review lands, and **Re-request review** puts them back. That is the whole trigger. A re-review moves the tree to the new heads and resumes the agent's earlier session, so it remembers what it said. It checks each point it raised before, and approves once everything it marked as blocking is fixed.
- **Cleans up.** A tree is removed once all its pull requests have merged or closed, or after `tree_ttl` (7 days) with nothing waiting.

Pushing new commits does **not** trigger a re-review on its own. Ask for one, as you would ask a person.

## Requirements

On the machine or in the image that runs the reviewer:

| | Why |
|---|---|
| `supatree` | The watcher and the reviewer. |
| [`nono`](https://nono.sh) | Every agent, and every git fetch into a review tree, runs inside a nono sandbox. On Linux nono uses Landlock, which needs kernel 5.13 or newer. |
| `claude` (Claude Code) | Runs the reviews. |
| `gh` | Polls pull requests and posts reviews, as the bot account. |
| `git`, `ssh` | Clones the repositories. |
| Your repositories' toolchains | The agent builds and tests every repository it reviews: Go, Node, Terraform, whatever your stack needs. Without them it can still review, but it cannot run the gate. |

You also need a **stack**: the registered set of repositories the reviewer covers. If you already use supatree you have one. Otherwise see the main README's `supatree scaffold` / `supatree stack clone`.

## Setting up the bot account

The reviewer needs a GitHub account of its own: a *machine user*. You cannot use your own account, because GitHub does not let you approve your own pull request. A GitHub App will not work either, because an App cannot be requested as a reviewer.

1. **Create the account**, e.g. `acme-review-bot`. Check your organisation's policy on machine users and their licence seat.
2. **Give it access** to every repository of the stack. *Write* access is what makes its approval count towards branch protection. Read access is enough if you only want its comments.
3. **Create a token** for it.
   - A **fine-grained token** scoped to those repositories, with:
     - *Pull requests*: read and write
     - *Contents*: read
     - *Metadata*: read
   - Or a **classic token** with the `repo` scope.

   The reviewer reads the token from `GH_TOKEN`, which `gh` uses in place of a stored login.
4. **Give it a way to clone.** Stacks usually name repositories by SSH URL (`git@github.com:…`), so add an **SSH key** to the bot account. The private key goes wherever the reviewer runs. If your stack uses `https://` URLs instead, run `gh auth setup-git` as the bot so git authenticates with the token.
5. **Give Claude credentials:** an Anthropic API key (`ANTHROPIC_API_KEY`), or a long-lived Claude Code token from `claude setup-token` (`CLAUDE_CODE_OAUTH_TOKEN`).

The reviewer refuses to act unless `gh api user` reports the login configured as `reviewer.login`. This protects against posting reviews under the wrong name.

## Configuration

The reviewer is configured in `~/.config/supatree/config.yml`. The watcher re-reads that file every round, so a change takes effect within 30 seconds, without a restart.

```yaml
reviewer:
  enabled: true
  login: acme-review-bot          # the bot account; must be who gh is logged in as
  stacks: [platform]              # optional: only these stacks (default: every registered stack)
  model: claude                   # optional: models entry to review with (default: default_model)
  group_window: 5m                # wait this long after the newest request before making a tree
  llm_grouping: false             # ask Claude which lone pull requests belong together
  max_concurrent: 2               # reviews running at once
  max_attempts: 3                 # failed runs before giving up on a request
  timeout: 45m                    # per review run
  tree_ttl: 168h                  # remove a tree after this long with nothing waiting
  guidance: |                     # optional: advice added to every review
    Treat missing tests for new behaviour as blocking.
  guidance_file: review-advice.md # optional: the same from a file (relative to ~/.config/supatree)
```

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `false` | Turns the reviewer on. |
| `login` | none (required) | The bot's GitHub login. |
| `stacks` | all | Which registered stacks' repositories to watch. |
| `model` | `default_model` | The `models:` entry reviews run under. It needs `exec_args`, the headless invocation; the built-in `claude` entry has them. Codex is not supported for headless runs. |
| `group_window` | `5m` | How long a new group waits for siblings. |
| `llm_grouping` | `false` | Ask Claude about one author's pull requests in different repositories that nothing links. That call has no tools, no MCP servers and no session; the pull request text goes to it on stdin, and every grouping it proposes is still checked against the one-per-repository rule. |
| `max_concurrent` | `2` | Concurrent review runs. Each one builds and tests whole repositories, so size this to the machine. |
| `max_attempts` | `3` | Failed runs per request before the reviewer gives up. A new push or re-request starts over. |
| `timeout` | `45m` | Hard limit per run; the agent is killed after it. |
| `tree_ttl` | `168h` | How long an idle tree is kept, so a re-review can resume the same session. |
| `guidance` | none | Review advice added to every review. See [Review guidance](#review-guidance). |
| `guidance_file` | none | The same, read from a file: absolute, or relative to `~/.config/supatree`. Read every round; an unreadable file stops the reviewer with an error rather than reviewing without it. |

Other settings worth knowing:

- **`max_running_agents`** (default 3) caps the agents the watcher launches into tabs. Running reviews count towards it, so on a machine you also work on they hold back other launches; the reviewer itself is limited only by `max_concurrent`.
- **`notify_command`** decides where the reviewer's notifications go. The default is a macOS notification. On Linux or in a container, point it at something else — see the example in [`deploy/reviewer/config.example.yml`](../deploy/reviewer/config.example.yml).

## Review guidance

You can give the reviewer advice of your own — what matters in your code, what counts as blocking, what to ignore. It is added to every review brief, in a section the agent is told comes from the maintainers, at three levels:

| Level | Where | Typical content |
|---|---|---|
| Every review | `reviewer.guidance` and/or `reviewer.guidance_file` in the config | Tone and house rules: "be terse", "missing tests for new behaviour block" |
| The stack | `REVIEW.md` at the root of the stack repo | Cross-repo rules: "an API change must update the TypeScript client", "deploy order is infra → api → web" |
| One repository | `REVIEW.md` at the root of that repo | Repo rules: "migrations must be reversible", "no new dependencies without a note in the PR" |

All three apply together, in that order. Each is cut at 16 KiB.

Guidance refines *how* the agent reviews. It cannot lift the reviewer's own rules — one review per pull request waiting on it, nothing posted on the others, pull request content treated as data — and the brief says so.

**Where each is read from matters**, because guidance is instructions and a pull request must not be able to write its own:

- A repository's `REVIEW.md` is fetched from GitHub on the pull request's **base branch** — never from the pull request itself. A pull request that edits `REVIEW.md` changes the guidance only once it is merged. One API request per repository per review.
- The stack's `REVIEW.md` is read when the review tree is created, from the stack as published (trees are built from the stack's origin), before any agent has run in it; it is kept for the tree's lifetime. To pick up an edit, the tree has to be made again — `supatree rm` it, or wait for it to be reaped.
- The config's guidance is read every round, from a file agents can only read.

## Running it locally

### Next to your own supatree setup

The watcher that `supatree start` launches is the one that runs the reviewer. **But that watcher runs as you**: its `gh` is logged in as you, and the reviewer refuses to post as anyone but `reviewer.login`. Run the reviewer in a home of its own instead, as the next section describes.

### In a home of its own (recommended)

Give the bot its own `HOME`, so its config, state, clones and Claude settings are separate from yours:

```sh
export HOME=$HOME/reviewer-home            # or a separate OS user
export XDG_CONFIG_HOME=$HOME/.config XDG_STATE_HOME=$HOME/.local/state XDG_CACHE_HOME=$HOME/.cache
export GH_TOKEN=<the bot's token>
export ANTHROPIC_API_KEY=<key>             # or CLAUDE_CODE_OAUTH_TOKEN

supatree init                               # directories, config, nono profile, MCP server
supatree stack clone git@github.com:acme/platform-stack.git
$EDITOR ~/.config/supatree/config.yml       # add the reviewer: block above

supatree watch --headless                   # runs until stopped; logs to stderr
```

Remember to set `XDG_*`. Supatree reads them before `HOME`, so a value exported by your shell would point the bot at your own directories.

`--headless` means the watcher needs no zellij session and never exits for want of one. To run it unattended, use launchd, systemd or `nohup`. A second `supatree watch` in the same home exits quietly, so starting it from cron every few minutes is a valid way to keep it alive.

The log has a line for each thing the reviewer does: the account it posts as, each tree it creates or removes, each run starting and ending (with the path of the agent's own log, which you can `tail -f`), and each review confirmed on GitHub with its verdict and commit. After a round that changed something, it adds a summary:

```
reviewer: posting as @review-bot
reviewer: 2 pull requests waiting, next due 14:05
reviewer: created rv-billing-api for acme/api#41, acme/web#87
reviewer: reviewing acme/api#41, acme/web#87 in rv-billing-api
reviewer: 2 pull requests waiting, 1 review running
reviewer: rv-billing-api: agent exited 0 after 7m40s (log …/reviewer-….jsonl)
reviewer: rv-billing-api: acme/api#41 changes requested at 3f9c2a1d8e04 (https://github.com/…)
reviewer: rv-billing-api: acme/web#87 approved at 81b0e7c4a9d2 (https://github.com/…)
reviewer: nothing waiting
```

A round that changes nothing logs nothing.

Before switching it on, try the configuration with `enabled: false`:

```sh
supatree reviewer plan     # dry run: refreshes the listings, shows what it would do, does none of it
```

`plan` checks that gh is logged in as `reviewer.login`, polls the watched repositories, and prints the actions a round would take and how it groups the open pull requests into changes. It creates nothing, posts nothing and records nothing, so a request it sees for the first time is always still in its `group_window`; the output says when that group's tree would be made.

The reviewer watches **the repositories your stack names**. If your stack points at forks while pull requests are opened against the upstream repositories, it will see none of them; the stack has to name the repositories pull requests target.

Once it is enabled, check on it with:

```sh
supatree reviewer status
```

## Deploying it to a container or Kubernetes

[`deploy/reviewer/`](../deploy/reviewer/) holds an example `Dockerfile`, an entrypoint, an example config and Kubernetes manifests.

> **Untested.** These examples have not yet been run end to end in a cluster. Treat them as a starting point, and read [What to verify first](#what-to-verify-first).

### What the image does

On start, `entrypoint.sh`:

1. starts an `ssh-agent` and loads the bot's key from `/secrets/ssh/id`. The key goes through an agent because nono's profiles keep agents and git fetches away from key files but let them use the agent socket.
2. writes `config.yml` from the environment, if the volume has none yet.
3. runs `supatree init`.
4. clones every stack in `SUPATREE_STACKS` that is not registered yet.
5. `exec`s `supatree watch --headless`.

| Environment | |
|---|---|
| `GH_TOKEN` | The bot's token (from a Secret). |
| `ANTHROPIC_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN` | Claude credentials (from a Secret). |
| `REVIEWER_LOGIN` | The bot's login. |
| `SUPATREE_STACKS` | Space-separated stack repository URLs to clone and watch. |
| `REVIEWER_LLM_GROUPING` | `true` to enable the LLM grouping pass (first start only, see below). |

The environment only seeds `config.yml` on first start. After that, the file on the volume is authoritative. To change it, edit it (`kubectl exec … -- vi ~/.config/supatree/config.yml`): the watcher picks the change up on its next round.

### Kubernetes

[`deploy/reviewer/k8s/`](../deploy/reviewer/k8s/) has three files:

| File | What it holds |
|---|---|
| `secret.example.yaml` | The token, the Claude credential and the SSH key. Fill it in, or create the same Secrets from your secret manager. Never commit real values. |
| `pvc.yaml` | The bot's whole home: config, state, the repository cache and the review trees. It must persist: the state records which reviews were posted. Size it for one clone of every repository plus a worktree per open review. |
| `deployment.yaml` | One replica, `Recreate` strategy, running as a non-root user. |

**Exactly one replica.** The watcher refuses to run twice through a lock file on the volume, so two pods sharing a `ReadWriteMany` volume would not double-post, but a second replica would do nothing useful. `Recreate` stops the old pod before the new one starts.

**Resources.** Each review builds and tests whole repositories. Give the pod CPU and memory for `max_concurrent` of those at once.

**Toolchains.** Extend the image with whatever your repositories build with, or the agent cannot run their tests.

**Logs.** `kubectl logs` shows the watcher's log, and the example config sends notifications there too. Each review run's full transcript is under `~/.local/state/supatree/trees/<tree>/logs/` on the volume.

### What to verify first

- **nono in the container.** nono needs Landlock: kernel 5.13 or newer, and a seccomp profile that allows the `landlock_*` syscalls. Check `nono run --profile supatree-agent -- true` in the pod.
- **Credentials inside the sandbox.** The review agent posts through `gh` from inside its nono sandbox, and that sandbox blocks `~/.config/gh`. On a Mac, gh keeps its token in the keychain. In the container, posting works only if `GH_TOKEN` (and `ANTHROPIC_API_KEY`) reach the sandboxed process. Check this with a test review.
- **SSH through the agent socket.** Check that a review tree can be created: it fetches the pull request heads inside the sandbox.

## Using it

- **Ask for a review:** add the bot as a reviewer on the pull request. Add it directly: **a request to a team the bot belongs to is not picked up**.
- **Cross-repo changes:** open the pull requests with `create_prs` from a supatree. Every one then carries the change marker and links its siblings, and the reviewer reviews them as one. Without supatree, link the pull requests to each other in their descriptions, or push the same branch name everywhere.
- **Siblings opened later** join the existing review tree and are reviewed in context.
- **Drafts** are skipped until they are marked ready for review, while the request still stands.
- **Re-review:** push your fixes, then click **Re-request review**.
- **Approval:** the bot approves only when nothing it considers blocking remains and the checks are not failing. Its review body says which findings block and which do not.

## Monitoring and operating it

```sh
supatree reviewer plan              # dry run: what it would do now, and how it groups the open pull requests
supatree reviewer status            # every watched pull request: state, tree, last verdict, failures
supatree reviewer forget acme/api#42 # clear what it remembers about one pull request (failures, give-up)
supatree ls                          # the reviewer's trees are listed under Reviews, named rv-<repo>-<n>
supatree ps                          # running review agents
```

- **The watcher's log** goes to stderr under `--headless`, otherwise to `~/.local/state/supatree/logs/watch.log`. It records every tree created, run started and finished, and every failure.
- **Run transcripts:** `~/.local/state/supatree/trees/<tree>/logs/reviewer-<time>.jsonl`, the agent's full stream.
- **Notifications** go through `notify_command`:
  - `review_posted` is board tier: recorded, not pushed.
  - `review_failed` is desktop tier: pushed. It means the reviewer gave up on a pull request and a person should look.
- **Giving up and retrying:** a pull request the reviewer gave up on is retried as soon as it changes, i.e. a push or a re-request. `reviewer forget` retries it at once.
- **Stopping:** stop the watcher (SIGTERM). Running reviews are stopped with it and picked up again on the next start.
- **Upgrading:** replace the `supatree` binary. A running watcher restarts itself onto the new binary within two rounds. A review in flight at that moment finishes, but its result is only confirmed indirectly: GitHub no longer lists the request once the review has posted.

## Security

- **Reviews are posted in the bot's name** with its token. Keep the token scoped to the stack's repositories, with the permissions listed above.
- **Treat everything in a pull request as hostile input:** descriptions, comments, commit messages, code. The reviewer is built to keep that input from steering it:
  - The agent runs in a nono sandbox restricted to its own review tree.
  - Its brief contains no text from the pull request, only URLs and commit IDs.
  - It is told to report, never to obey, instructions found inside a pull request.
  - The LLM grouping call has no tools.
  - The reviewer's own record of what was posted lives where no agent can write it, and is confirmed against GitHub.
- **An approval is a claim** that the agent ran the gate and read the tree. If your branch protection counts the bot's approval, it is effectively a required reviewer you have automated. Decide deliberately whether it should count, or keep it read-only so that its reviews are comments only.
- **The agent runs your repositories' code** (builds and tests) inside the sandbox, as any CI would. Do not point it at repositories whose pull requests can come from untrusted forks unless you are comfortable with that.

## Limitations

- When it first starts on a repository, the reviewer sees requests among the 100 most recently updated pull requests. An older, untouched request is picked up once the pull request next changes.
- Review requests to teams are not recognised; request the bot itself.
- Only repositories in a registered stack that supatree has already cloned are watched.
- The agent works from the code; it has no access to your CI's logs beyond what `gh pr checks` shows.
