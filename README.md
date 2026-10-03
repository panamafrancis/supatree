# supatree

Multi-repo worktrees for a single cross-repo issue, each agent in a
[nono](https://nono.sh) sandbox.

## Install

```sh
go install github.com/panamafrancis/supatree/cmd/supatree@latest
supatree init
```

Requires git, [zellij](https://zellij.dev), nono, and `gh` for pull requests.

`supatree` is for work that spans several repos at once. It is built on [workbench](https://github.com/panamafrancis/workbench)'s packages — supatree is workbench for multi-repo changes — but independent of it at runtime: either installs and runs without the other, and they share no file on disk. Think of a change to `terraform`, then `keystone-api`, then `admin-frontend`. It creates one worktree per repo under a shared root so a single agent can run at the top and see every repo, and it orchestrates per-repo PR creation.

## Model

- A **stack** is a git repo holding the repo selection (`supatree.yml`), an agent guide (`AGENTS.md`), and `scripts/`. Create one with `supatree scaffold`. It lives wherever you like — `~/supatree/stacks/` by default — and is self-describing: `supatree.yml` maps each member's alias to its git URL, so a teammate who clones the stack can use it as is.

  ```yaml
  members:
    keystone: git@github.com:fraud-zero/keystone.git
    admin-frontend: git@github.com:fraud-zero/admin-frontend.git
  deps:
    admin-frontend: [keystone]
  ```
- **Supatree owns its clones.** Each member is cloned once into supatree's repo cache, `~/supatree/repos/<host>/<owner>/<repo>/`, from the URL the stack gives, and every tree's worktree of it hangs off that clone. Supatree never uses a clone it did not make (your `~/code/…`, workbench's repos). A personal ssh alias (a second GitHub account) belongs in git's `url.<base>.insteadOf`, not in the shared spec.
- A **supatree** is a worktree of that stack repo at `~/supatree/trees/<name>/`, with each member repo checked out under `repos/<alias>/`. Its state (`meta.yml`, `agents.yml`, `info.md`, `board.md`, mailboxes) lives in `~/.local/state/supatree/trees/<name>/`, and the tree's `.supatree` is a link to it — so `.supatree/info.md` reads as it always did, and a sandbox can be given every tree's state without being given anything inside a tree. It is named with a city name, like workbench worktrees.
- Personal files a worktree needs but git does not track — a `.env` with credentials — are `copy_files`, set per repository in `~/.config/supatree/config.yml` and copied from the cache clone's checkout into every new worktree. Put the file in the cache clone once; every tree gets a copy. A listed file that is missing is a warning, never a prompt:

  ```yaml
  repos:
    github.com/fraud-zero/keystone:
      copy_files: [.env.local]
  ```
- A stack's `scripts/setup`, if present, runs once per new tree — after the members exist and `copy_files` has run, before any agent tab opens — with stdin closed, so it can never wait on a person; it gets `SUPATREE_NAME`, `SUPATREE_ROOT` and `SUPATREE_MEMBERS`, its output goes to `.supatree/setup.log`, and a failure is a warning on a tree that still works. `sync` adding a member later runs that member's `copy_files` but not `setup` again.
- Member branches are `st/<slug>/<alias>` (the slug starts as the city name; rename it before opening PRs). The stack worktree itself is on `st/<name>`.
- A **review tree** (`supatree review`) is the same thing pointed at someone else's work: each member is checked out at a pull request's head on a tree-local `review/<slug>/<alias>` branch, and the tree records the PRs rather than deriving them from the branch name. The authoring commands (`rename-branch`, `create_pr`, `create_prs`) refuse there — the branches belong to the PRs' authors — and `.supatree/info.md` carries review instructions instead. The `docs` MCP tool with `topic: review` explains how to review in one. A review tree finishes as `reviewed` rather than `done` — the author's merge is their milestone, not work you shipped, and `history` counts the two separately.
- **Review tooling.** `review_refresh` (MCP, or `supatree review refresh`) re-fetches the PR heads when an author pushes; `review_post` submits one batched review with inline comments anchored to the checked-out commit, and refuses if the head has moved since. Posting publishes in your name, so it requires the `outward` permission (off by default at every autonomy level). `supatree review fork` converts a review tree into an authoring one whose PRs target the authors' branches.
- Teardown (`supatree rm`, and `sync --prune`) deletes only the branch the tree itself created. A member left on some other branch — after a manual `gh pr checkout`, say — is reported and left alone, because the delete is `git branch -D`.
- Dependencies between repos (`deps:` in `supatree.yml`) drive creation order, the merge order shown in `.supatree/info.md`, and `create_prs` ordering.

## Quickstart

```sh
supatree init                                   # dirs, config, supatree-agent nono profile, MCP
supatree stack new fraud --repos=fraud-zero/terraform,fraud-zero/keystone  # clones into the cache
# (or --from ~/code/fraud-zero to pick from local checkouts' origins, --from-org fraud-zero via gh;
#  members are owner/repo, a git URL, or alias=<either>; `scaffold` is an alias of `stack new`)
supatree stack dep fraud keystone terraform     # keystone's PR merges after terraform's
supatree stack add fraud fraud-zero/admin-frontend --as admin
supatree stack rm  fraud admin
supatree stack clone git@github.com:you/fraud-stack.git   # adopt a teammate's stack
supatree repo ls                                # the repo cache: clones, size, trees using each
supatree repo rm fraud-zero/keystone            # refuses while a tree has a worktree of it
# edit ~/supatree/stacks/fraud/supatree.yml to add deps, commit it
supatree start                                  # start the st-main Zellij session
supatree new --stack=fraud                      # create a city-named supatree
supatree open <name>                            # open the root agent (sees all repos)
supatree open <name> --agent=reviewer           # a second, independently-resumable agent
supatree ls                                     # list supatrees + member PR status
supatree status                                 # activity of every supatree (open/pushed/approved/stale/done)
supatree dash                                   # full-screen dashboard of the same
supatree watch                                  # background poller: activity ledger + desktop notifications
supatree comments <tree> <repo>                 # what reviewers said, with unresolved threads first
supatree pm                                     # the PM agent — sees every supatree at once
supatree request "…"                            # queue something for the PM
supatree schedule                               # recurring PM work (standups, triage, reminders)
supatree message <tree> <agent> "…"             # leave a message in an agent's mailbox
supatree inbox <tree> <agent>                   # what is waiting for it
supatree sync <name>                            # reconcile after editing supatree.yml
supatree rename-branch <slug> <name>            # rename all member branches (slug: max 40 chars)
supatree rm <name>                              # tear down all member worktrees

# Reviewing someone else's cross-repo change
supatree review <pr-url> <pr-url> ...           # review tree: each repo at its PR head
supatree review fraud-zero/api#600 fraud-zero/web#988   # or owner/repo#number
supatree review refresh <name>                  # authors pushed — re-fetch the heads
supatree review fork <name>                     # turn the review into a proposal
```

**When an agent tab will not start:** `supatree doctor` checks zellij, git, nono and every configured model's nono profile, including each profile it extends. Opening an agent or the PM checks its profile first and fails with nono's complaint; and an agent pane whose nono exits within a few seconds stays open on nono's output until you press enter (each launch is logged to `~/.local/state/supatree/logs/agent-exec.log`). After a nono upgrade, `nono outdated`, `nono list --installed` and `nono profile list` show what changed.

`rename-branch` renames every member or none: if a member fails, the renames already made are rolled back, so the tree's recorded slug never points at a name only some members are on. With `--push` the pushes run after every local rename has landed, and a push failure is reported per repo without undoing the rename.

## Sidebar

`supatree ls` (the sidebar in each supatree tab, and `supatree start`'s pane) is a TUI listing every supatree with its agents and member repos.

The **PM** has a section of its own pinned at the very top, above a divider — `◆ PM`, with `●` while its tab is open and `✉N` for requests it has not read yet. It is there even before any supatree exists; `gg` then `enter` (or `P` from anywhere) opens or focuses it.

| Key | Action |
| --- | --- |
| `j` / `k` (or `↓` / `↑`) | Move down/up (skips subheaders) |
| `Ctrl+d` / `Ctrl+u` | Half-page down/up |
| `gg` / `G` | Jump to the first / last row |
| `}` / `{` (or `]` / `[`) | Jump to the next / previous supatree |
| `Space` | Fold/unfold the innermost section — the repositories list on a repo row, otherwise the supatree |
| `h` / `l` (or `←` / `→`) | Collapse / expand; `h` closes the repositories section first, then the supatree |
| `zM` / `zR` | Fold / unfold **every** supatree |
| `Enter` / `o` | Open the selected agent, a shell in the selected member repo, or the PM on the PM row |
| `a` | Add an agent to the selected supatree — on a member row, open that repo's agent |
| `n` | New supatree |
| `s` | Sync the selected supatree |
| `d` | Delete the selected supatree |
| `D` | Open (or focus) the dashboard tab |
| `P` | Open (or focus) the PM agent |
| `m` | Hand the selected row to the PM |
| `r` | Refresh (forces a PR status fetch) |
| `?` | Keybinding reference (any key closes it) |
| `q` | Quit (confirms in sidebar mode) |

The mouse works too: the wheel scrolls the list and a click selects a row. Wheel scrolling pans the view without moving the cursor, so you can read further down the list and it stays put — the view snaps back to the cursor as soon as you press a movement key. When the list is taller than the pane it scrolls to keep the cursor in view.

Pressing `n` prompts for a **name** (leave it blank to auto-generate a city name). If more than one stack is registered you first pick which stack from a list (`↑`/`↓` or `j`/`k` to move, `enter` to select, `esc` to cancel), then the name. The name is checked as you type — a name that would be rejected (`feature-v1.1`, or one already taken by a supatree) shows the reason under the field and `enter` leaves the prompt open so you can fix it in place; the warning clears with the character that caused it. After creation the cursor lands on the new supatree so it scrolls into view.

Each supatree's **repositories section starts folded**, so a long list of supatrees stays readable. Its header carries a coloured count per PR status instead (`◌1 ◉2 ✓1 ✕1 ·3` — draft, open, merged, closed, and members with no PR yet); the same summary moves up onto the supatree row when the whole supatree is folded. Unfold the section (`Space`, `l` or `enter` on the `repositories` row) to see the member rows, which show each repo's PR state and number in full (`◉ open #871`).

`S` creates a stack: it asks for a name, then runs `supatree stack new <name>` (its picker over the repo cache) in a tab of its own.

Folds are shared: they live in `~/.local/state/supatree/ui.yml` rather than in each sidebar process, so folding a supatree in one tab folds it in every other tab's sidebar on its next reload (focus or the 30s tick) instead of leaving each tab with its own shape of the same list.

The two sections answer different questions, so `enter` does different things in them. Agent rows are processes (the `●`/`○` dot is liveness), and `enter` opens or focuses that agent's tab. Member rows are places reporting state (branch, dirty mark, PR), so `enter` (or `o`) stands in one: a shell pane rooted at `repos/<alias>/`, opened beside the agent in the tab's main area rather than under the sidebar. That shell is your own — it is **not** inside the nono sandbox, unlike every agent workbench and supatree launch — and it is disposable, closing when you exit it. To get an agent scoped to a single member repo instead (nono allows only that repo, not the whole tree), press `a` on the member row; `a` on a supatree or agent row still prompts for a new root agent's name. The footer hint tracks the cursor (`enter shell` vs `enter open`) so you can see which you'll get. A member that isn't checked out yet says so and points at `supatree sync`.

Like the workbench sidebar, each supatree tab's sidebar marks the supatree that tab belongs to with a `▸` in the gutter ("you are here"), independent of the cursor. It re-reads live state when the pane regains focus and on its periodic tick, so newly created or removed supatrees appear across tabs without pressing `r`. PR status is fetched under the same quota discipline as the workbench sidebar — one conditional poll per repo (free when nothing changed), single-fetcher lock, no request for unpushed branches, and a persisted pause until the reset time a rate-limit response reports — so a churning or multi-tab sidebar doesn't drain the API quota. Both sidebars share the same code path (`github.Sync`).

## Status and dashboard

`supatree status` answers "where is everything?" across all supatrees at once. It derives each member repo's place in the ship lifecycle from local git plus the cached PR status:

```
absent → idle → wip → pushed → draft → open → changes | approved → merged | closed
```

and rolls that up per supatree — `setup` (a member worktree is missing), `new`, `wip`, `pushed`, `review`, `approved` (every open PR approved, ready to merge), or `done` (everything merged or closed, so the tree is only occupying disk — the cue to `supatree rm` it). Alongside the state it flags supatrees that are **blocked** (a PR has changes requested or failing checks), **dirty** (uncommitted work), and **stale** (no commit in any member for `--stale-after`, default 7 days).

```sh
supatree status              # human-readable, cache only — costs no GitHub quota
supatree status --refresh    # fetch PR status from GitHub first
supatree status --json       # the whole summary, for scripts and watch loops
supatree status --all        # include the members of finished supatrees
```

`supatree dash` is the same data as a full-screen TUI, meant to live in its own window or Zellij tab: one row per supatree with its state, open/total PRs, review verdicts (`2✓ 1✗ 3·` — approved, changes requested, waiting), time since the last commit, and what needs attention. `space` expands a supatree to its member repos with PR numbers and per-repo state; `enter` focuses that supatree's Zellij tab. The most actionable supatrees sort first (blocked, then ready to merge, then in review) and finished ones sink to the bottom.

Press `D` in the supatree sidebar to open or focus the dashboard in a `supatree-dash` tab, or run `supatree dash` in any terminal. Both the dashboard and the sidebar read the same on-disk PR cache and fetch under the same staleness gate, cross-process lock and rate-limit backoff, so running a dashboard alongside a screenful of sidebars adds no extra GitHub API load. Non-interactive (piped) output falls back to `supatree status`.

## Review comments

`supatree comments <tree> <repo>` shows what reviewers have said on a member repo's pull request: top-level comments, review verdicts, and line-anchored threads with their **resolved state**. Unresolved threads lead the output, because they are the only part still waiting on an answer; `--all` covers every member, `--json` is for scripts, `--force` re-fetches.

Unlike `supatree status`, this one costs API quota. Thread resolution exists only in GitHub's GraphQL API, so it is a second query shape per PR on top of the one the sidebar already spends. It is therefore fetched **only when asked for, never on a timer**, and cached until the PR itself changes — so asking twice about an unchanged PR is free. Agents get the same thing as the `pr_comments` MCP tool.

## The PM agent

`supatree pm` (or `P` in the sidebar) opens a standing agent rooted at `~/.local/state/supatree/pm` that can see every supatree at once: what is blocked, what reviewers said, who is working where. It is **optional** — nothing else depends on it running, and without it supatree behaves exactly as it does today.

It is not rooted in a supatree, because one that manages many cannot live inside one of them. That also means it gets its own MCP gate: `SUPATREE_PM=1` unlocks the cross-tree tools (`requests`, `list_trees`, `events`, `notify`), while `SUPATREE` stays *unset* so the tree-scoped tools stay hidden rather than resolving nothing.

**Its sandbox is a different shape, not a bigger one.** It allows its own home, every tree's state (one directory, so a tree created after the PM started needs no relaunch), the notification outbox and the stack repos — and nothing inside any tree. The invariant is not "read-only on trees" but *the PM may write supatree's own state and never a member repo's working tree*. Creating and removing trees writes exactly what it may not, so `new_tree` and `remove_tree` ask the watcher to do it and wait for the answer. Set `pm_model` in `~/.config/supatree/config.yml` to point it at a `models` entry with its own `nono_profile`: the PM executes no third-party code but reads text other people wrote and holds credentials that reach off the machine, so the profile it wants is narrow on egress rather than wide on the filesystem.

```yaml
# ~/.config/supatree/config.yml
pm_model: claude-pm     # a models entry whose nono_profile scopes the gh credential
```

**Reaching it.** Anything that can append to a file can queue a request — `supatree request`, the sidebar, the watcher — and the PM reads the queue at the top of each turn. That indirection is the point: a Go process cannot use a message bus, but every Go process can append a line. The queue is read from a **stored offset**, so a PM that has been closed for a day catches up on the backlog instead of losing it, and the offset is only committed after the requests have been handed over.

**Two rules it is given up front.** It never opens a Zellij tab unprompted — opening focuses the tab and yanks the terminal away from whoever is using it, so it creates trees and *reports*, and you press enter yourself. And it treats fetched text as data rather than instructions: a PR comment is written by anyone who can comment on the repository.

It also cannot notify you directly — nothing inside the sandbox can — so its `notify` tool queues through the watcher, which applies the same tiering and deduping as its own events.

## Autonomy

What the PM may do unasked is explicit and per supatree, in `.supatree/meta.yml`, with a workspace default in `~/.config/supatree/config.yml`:

| Level | Unasked, the PM may | If you ask it to |
| --- | --- | --- |
| `off` | report only | report only |
| `nudge` *(default)* | message agents | create, reap, push |
| `auto` | create supatrees, open PRs, reap finished ones | as unasked |

**The level governs what the PM does unasked**, which is the only thing about it worth being careful over. Ask it to create a supatree and it creates one: the mutating tools take an `asked` flag, the PM sets it when the request came from you in that turn, and below `auto` that is the difference between doing the thing and reporting that it could. `off` is the exception — report-only means report-only, and asking does not lift it — and a scheduled turn cannot carry the flag at all, because there is nobody in one to have asked. It is the same assertion `remove_tree`'s `force` has always rested on, trusted the same way: autonomy is a consent boundary and the sandbox is the security one, and consent is exactly what an agent is in a position to report.

**Outward-facing actions are a separate axis** (`outward`, off everywhere by default). "Message a local agent" and "comment on a PR" are different kinds of risk — one is private and recoverable, the other is published and permanent — so wanting the PM to create supatrees unattended does not also grant it a public voice.

The workspace default is not just convenience: autonomy lives per tree, so without it nothing would govern `new_tree`, which has no tree yet to carry a level. **A scheduled turn caps at `nudge`** however the tree is configured, unless its schedule entry opts in — nobody is watching one of those.

## The board

`.supatree/board.md` is what the PM maintains per supatree: what each agent is on, what is blocked, what is waiting on you. Status must not mean "read the PM's chat log" — scrollback is a terrible status display and people stop reading it by day three. Chat is where you negotiate; the board is where you check. `b` in the dashboard shows the selected tree's board.

## Memory

Durable notes live in `notes/` **in the stack repo**, scaffolded by `supatree scaffold`. That is a deliberate choice over a vector store: at the volume this produces — tens to low hundreds of finished supatrees a year — grep beats embedding retrieval on precision and on being debuggable, and a git directory is diffable, blameable, reviewable and shared with the team. Curation arrives as a pull request rather than a migration.

Three tools: `remember` writes a note, `recall` searches them, and `history` answers what shipped from the event ledger. The split matters — **`history` is exact and `recall` is not**, so the PM is told never to answer a status question from memory. Git and the ledger are authoritative for facts; notes are for judgement.

Two things keep it from rotting. `info.md` names only the few most recent notes, with everything else behind `recall`: storage was never the hard problem, what loads into every session is. And every `recall` logs its query and whether it hit, so *"a store nothing has read in 30 days gets deleted, not debugged"* is a measurable claim rather than a hope.

`supatree rm` no longer throws away agent history either: transcripts are moved to `~/.local/state/supatree/archive/<tree>/` before the session cache is cleared. Archiving is deterministic and cheap, which is what makes it safe on the removal path — distilling one into something worth keeping is a judgement call, and blocking a removal on an agent round-trip would be worse than the leak.

## Scheduled work

`~/.config/supatree/schedule.yml` (`supatree schedule init` writes an example) runs recurring PM work: a morning standup, hourly triage of new review comments, a Friday reap proposal, or a one-shot reminder.

The scheduler lives in the **watcher**, not in the PM. An agent cannot be trusted to hold a timer — it is mid-turn, blocked on a tool call, or was restarted an hour ago — and a schedule that silently drops jobs is worse than none. Firing a job is an append to the same request queue the sidebar's `m` uses, so the PM needs no timer and no new channel.

```yaml
jobs:
  - id: standup
    at: "09:00"
    days: [mon, tue, wed, thu, fri]
    when: events_since_last     # the default: stay silent when nothing moved
    prompt: "Summarise what moved since yesterday."
```

Three rules do the real work. A job seen for the first time is **seeded, not fired** — otherwise writing the file fires every entry at once. A job that missed fifteen hourly windows overnight fires **once**, not fifteen times. And `when: events_since_last` is the default because a standup that reports "nothing changed" every morning is notification fatigue wearing a suit. Jobs read the cache and never fetch, so a timetable costs no API quota.

`supatree schedule run <id>` fires one now for testing, deliberately without touching the fire times.

## Talking between agents

Several agents can share a supatree, and they can now reach each other. Each is launched with a stable address — `st-<tree>-<agent>` — recorded in `.supatree/agents.yml` and passed to the CLI via the model's `agent_name_args` (`["--name", "{agent_name}"]` for claude). Without it every agent in a tree would derive its name from the shared tree root and they would all collide.

Three MCP tools: `agents` lists who is here with their addresses and unread counts, `message_agent` leaves one a message, and `inbox` reads and clears your own. All three take an optional `tree`, so the PM — which lives in no supatree — can use them by naming one; inside a supatree you can omit it and mean your own.

**Delivery is always by mailbox** — a file under `.supatree/mail/<agent>/`, read on the recipient's next turn. That is the contract, and it works for every model, whether or not the recipient is running. A message bus, where the CLI has one, only makes the same message arrive sooner; `agents` reports per agent whether it is reachable that way (`bus st-canberra-main`) or by mailbox alone (`mailbox (next turn)`), rather than implying parity.

```yaml
# ~/.config/supatree/config.yml — a model with no message bus simply omits this
models:
  claude:
    agent_name_args: ["--name", "{agent_name}"]
```

## Notifications

`supatree watch` is the single background poller. Each round it refreshes PR status on the shared staleness gate, derives the same summary `supatree status` shows, diffs it against the previous round, appends what changed to the ledger (`~/.local/state/supatree/ledger/events.jsonl`), and delivers the few events that warrant interrupting you as desktop notifications.

`supatree start` spawns one automatically and it exits when the last supatree Zellij session closes. It is a singleton enforced by a file lock, so a second one — a stray `supatree watch`, or a cron entry firing while a session is open — exits quietly rather than doubling the GitHub API load. That makes a scheduled `supatree watch --once` safe to add if you want the hours when no session is running covered too.

Events are a diff, not a report. Nothing that has no previously observed state is announced, so a fresh install and a newly created supatree both stay quiet instead of telling you about everything they can see.

Only two kinds interrupt you: **changes requested** and **checks failing** — the two that mean a human is now waiting on you. Merges, approvals and finished trees are recorded but silent, and pushes and opened PRs only change a sidebar glyph. Repeats of the same news stay quiet for a cooldown (two hours for failing checks, which flap as CI re-runs), and notifications for the supatree whose tab you are currently looking at are suppressed, since you can already see it.

```yaml
# ~/.config/supatree/config.yml
notify_command: ["notify-send", "{title}", "{text}"]   # default: osascript on macOS
watch_interval: 30s                                    # how often to re-derive; the gh fetch behind it stays gated
```

`{title}` and `{text}` are substituted; the values are stripped of quotes and control characters, so a PR title cannot break out of the notifier's own quoting.

In the sidebar, a supatree holding news you have not looked at is marked `!` next to its name; opening it clears the mark. In the dashboard, `e` toggles a recent-activity feed of the same ledger.

## Agents

Opening a root agent also marks the tree root as a trusted folder in `~/.claude.json`, so Claude does not ask "Do you trust the files in this folder?" on every launch. It has to be seeded rather than simply answered once: several agents share the tree root, each rewrites that file wholesale from what it read at startup, and an agent that started before you accepted puts the unaccepted answer back. Only the `hasTrustDialogAccepted` flag for the tree root is touched, only when it is not already set.

All agents run at the supatree root under a nono sandbox that allows the whole tree, its state, and the git directories its commits land in (the stack's and each member's clone) — not other trees, not the PM's queue, not supatree's own state. A member that `sync` adds is not committable by agents already running until they restart: a sandbox's reach is fixed when it starts. `sync` itself is carried out by the watcher, since adding a member writes a clone the agent's sandbox does not reach. Multiple named agents (`--agent`) share the directory but resume independently via cached session IDs. `--repo <alias>` opens an agent scoped to a single member repo instead (the same thing `a` does on a member row in the sidebar).

## MCP tools

`supatree init` registers an MCP server (`claude mcp add supatree -s user -- supatree mcp`) — **run it before your first supatree**, otherwise the supatree PR tools won't appear and you'll only see workbench's own tools. Inside a supatree, the agent gets: `supatree_info`, `sync`, `rename_branches`, `create_pr`, `create_prs` (dependency-ordered), `pr_status`, and `docs`. PR tools refuse to run while the branch slug is still an auto-generated city name, and (unless forced) while a repo's dependencies have no PRs yet. Tools gate on the `SUPATREE` env var, so global registration is safe. New branch slugs (`rename_branches` / `rename-branch`) are lowercase alphanumeric and hyphens, max 40 chars.

The **workbench** MCP tools (`create_pr`, `rename_branch`) are for plain workbench worktrees, not supatrees: inside a supatree they detect `SUPATREE=1` and redirect you to the supatree tools above rather than acting on the wrong branch.

