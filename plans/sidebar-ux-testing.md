# Sidebar UX testing — teatest, a sandbox, and the scenarios

## Context

The sidebar's tests drive `Model.Update` by hand and inspect the model or a
`View()` string (`pkg/supatree/tui/model_test.go`). That covers logic. It does
not cover the program as it runs: `Init`'s commands, async results landing
between keystrokes, a real terminal size, and what the screen looks like. The
e2e scripts only exercise the command line.

A first pass driving the real sidebar in a headless terminal (ht) found four
bugs in an afternoon, all at the default 28-column width: `main` missing beside
a named agent, failed launches left registered, clipped help and prompts, and
errors filling the pane. None of them would have been caught by the existing
tests. This plan makes that kind of testing routine.

There are three layers, from cheapest to most real:

| Layer | Runs | Catches | Where |
|---|---|---|---|
| **teatest** | `go test`, CI | behaviour and layout regressions, deterministically | `pkg/supatree/tui/*_teatest_test.go` |
| **Sandbox + ht** | by hand, or by an agent | what it is like to use; things no one thought to assert | `scripts/ux-env.sh` |
| **e2e** | `make e2e`, CI | the CLI lifecycle (exists) | `scripts/e2e-supatree.sh` |

## Layer 1 — teatest

`github.com/charmbracelet/x/exp/teatest` runs a `tea.Model` in a real
`tea.Program` with a fixed terminal size. You send keys, wait for output, and
compare the final screen against a golden file.

### Setup

- **Dependency.** Add `github.com/charmbracelet/x/exp/teatest`, pinned to a
  version that targets bubbletea v1. It is test-only.
- **Fixed colours.** Call `lipgloss.SetColorProfile(termenv.Ascii)` in
  `TestMain`, so goldens hold plain text with no escape codes. A small number of
  tests keep the ANSI profile, to assert on the selection highlight (the cursor)
  the way `ux-env.sh shot` does.
- **Fixed size.** `teatest.WithInitialTermSize(28, 40)` is the default
  sidebar. Width-sensitive scenarios also run at 20 and 60 columns.
- **Goldens.** Kept in `testdata/*.golden` and regenerated with
  `go test ./pkg/supatree/tui -update`. A golden diff in review is the visual
  change.

### Keeping it hermetic

`Init` and the ticks call out to real binaries:

| Call | Binary |
|---|---|
| `refreshRunningCmd` (`zellijTabs`) | zellij |
| `fetchPRCmd` | gh, through the PR cache |
| `refreshDirtyCmd` | git |
| opening an agent or PM | zellij, nono |

The package tests already handle this with fake binaries on PATH
(`fakeNono` in `agentexec_test.go`). Do the same here: a `fakeBin(t, name,
script)` helper, plus fakes that are on by default:

- **zellij** answers `query-tab-names` from a file the test controls, and logs
  every other action to a file the test can assert on (e.g. "a `new-tab` named
  `oslo:reviewer`").
- **gh** fails as if unauthenticated. PR scenarios seed the cache file
  (`PRCachePath()`) instead, so nothing calls gh.
- **nono** is `fakeNono`, with a parameter that makes a profile broken.

This needs no production change. If fake binaries prove too slow or brittle,
the fallback is a small `deps` struct on `Model` (tabs, PR fetch, open agent,
open PM) with real defaults, but only once a test needs it.

**Ticks.** `tickInterval` is long enough that a test finishes first. Scenarios
about periodic refresh send `tickMsg{}` themselves rather than waiting.

**Fixtures.** Use real trees made through the `supatree` package (scaffold, then
`New`) in the isolated HOME `TestMain` already provides, rather than
hand-assembled `Instance` values. That way `reload()` reads them the way it
does in production. Reuse the shapes from `scripts/ux-env.sh setup`.

### Harness shape

```go
tm := startSidebar(t, fixture{trees: wipAndFresh, width: 28})
tm.Type("j")
waitFor(t, tm, "enter shell")
tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
assertZellijCalled(t, "new-pane")
tm.Quit()
teatest.RequireEqualOutput(t, finalScreen(tm))
```

`startSidebar` builds the fixture, installs the fakes, and returns a
`*teatest.TestModel`. `waitFor` wraps `teatest.WaitFor` with a short timeout
and prints the screen on failure.

### What stays as direct `Update` tests

The pure logic stays as it is: row building, fold state, cursor clamping, name
validation. Those tests are faster and say more precisely what broke. teatest
is for what only shows up in the running program, or on screen.

## Layer 2 — the sandbox (`scripts/ux-env.sh`)

This is built. It provides:

- an isolated HOME, XDG directories and zellij socket directory;
- `supatree init`, so the nono profile is installed;
- three fixture repos with dependency edges, and a stack;
- trees in different states:
  - **berlin:** fresh;
  - **paris:** in progress, with a commit on one member, uncommitted edits on
    another, and a named agent beside `main`;
  - **oslo:** renamed off its city name;
  - optionally `--trees N` extras, for scrolling;
- a second PM;
- a fake `claude` that prints how it was launched and echoes input.

```
scripts/ux-env.sh setup [--trees N]
scripts/ux-env.sh start              # in a terminal, or inside ht
scripts/ux-env.sh focus left
scripts/ux-env.sh keys j j Enter
scripts/ux-env.sh shot               # pane text, ">> " on the highlighted row
scripts/ux-env.sh rebuild            # new binary, sidebar restarted on it
scripts/ux-env.sh teardown
```

What the first run taught about driving it headless (also in the script's
header):

- zellij draws nothing until its client gets its first input.
- Text snapshots lose the cursor; `shot` reads zellij's ANSI dump instead.
- Escape followed by a key in one burst reads as Alt+key.
- Agent tabs need nono, which cannot nest, so they fail from inside another
  agent's sandbox.

The sandbox's tree shapes are also the fixtures teatest should build, so both
layers describe the same world.

### Later

- Seed PR states (open, approved, changes requested, merged) by writing the PR
  cache. This needs the cache format pinned in a helper both layers share.
- A review tree, made from the seeded PRs.
- A `make ux` target, and possibly a `vhs` tape per scenario, to record GIFs
  for PRs.

## Layer 3 — e2e

No change. If teatest's fake zellij turns out to miss real behaviour, add one
scenario to `e2e-supatree.sh` that starts a detached zellij session, drives it
with `zellij action write-chars`, and asserts on `dump-screen`. The sandbox
script already does all of this except the assertions.

## Test scenarios

Layer: **T** teatest, **H** sandbox/ht (exploratory), **E** e2e.
Status: **✓** covered today by a direct `Update` test (it may still deserve a
teatest version), **new** not covered.

### Layout and width

| # | Scenario | Layer | Status |
|---|---|---|---|
| 1 | Default 28-col render of the WIP fixture matches its golden | T | new |
| 2 | At 20 / 28 / 60 cols no line is wider than the pane: list, footer, help, prompts, errors | T | ✓ partly (help, prompts, errors at one width) |
| 3 | Resize while running (`WindowSizeMsg`) reflows the footer and help and keeps the cursor in view | T | new |
| 4 | A pane shorter than the list scrolls with the cursor; `ctrl+d` / `ctrl+u` page by half the pane | T | ✓ |
| 5 | A very long tree or agent name is truncated or folded consistently, never pushing badges off-screen | T, H | new |
| 6 | Footer hint names what `enter` does on the current row (agent / shell / PM) | T | ✓ |

### Navigation

| # | Scenario | Layer | Status |
|---|---|---|---|
| 7 | `j` / `k` skip headings, subheaders and the divider | T | ✓ |
| 8 | `gg` / `G` go to first / last; `}` / `{` jump between trees | T | ✓ |
| 9 | Escape and then a key, sent separately, behave as two keys (no stray Alt chord) | T, H | new |
| 10 | Mouse: click selects a row; wheel scrolls without moving the cursor | T | ✓ |
| 11 | A half-typed `g` or `z` is cleared by any non-matching key, including `e` | T | ✓ |

### Folding and sections

| # | Scenario | Layer | Status |
|---|---|---|---|
| 12 | Repositories start folded with a PR-count badge; `space` / `l` unfold | T | ✓ |
| 13 | `zM` / `zR` fold / unfold all; fold state is shared with other sidebars through disk | T | ✓ |
| 14 | Sections: Product Managers, WIP, Reviews, and the creating placeholder, in order | T | ✓ |
| 15 | A tree being created shows a placeholder until it lands, then the cursor moves to it | T | ✓ |

### Agents

| # | Scenario | Layer | Status |
|---|---|---|---|
| 16 | `main` is listed first beside named agents, whether or not it is recorded | T | ✓ |
| 17 | `enter` on an agent opens its tab: the fake zellij sees `new-tab <tree>:<agent>` | T | new |
| 18 | `a` prompts for a name, validates it as you type, and opens the agent | T | new |
| 19 | `a` with nono refusing the profile: compact error, `e` shows nono's output, no record left | T, H | ✓ (rollback in the package test; screen new) |
| 20 | `a` where the tab open itself fails: the record stays | T | ✓ (package test) |
| 21 | `d` on a named agent asks to confirm (`[y/N]` visible at 28 cols), then removes it and closes its tab | T | ✓ partly (tab close new) |
| 22 | `d` on `main` refuses, and the whole message is visible | T | ✓ |
| 23 | Running agents are marked from zellij's tab list; a tab that goes away is unmarked on the next refresh | T | new |
| 24 | `enter` on a member row opens a shell pane there | T | ✓ |

### PMs

| # | Scenario | Layer | Status |
|---|---|---|---|
| 25 | `p` adds a PM below the others and opens it | T | ✓ partly |
| 26 | `p` with a broken profile: the PM is not registered and the error is compact | T | ✓ (package test; screen new) |
| 27 | `d` on the only PM refuses; on the top PM it promotes the next | T | ✓ |
| 28 | Pending-request counts per PM update on refresh | T | ✓ |
| 29 | `m` hands the selected row to the top PM (a request is queued with the right text) | T | new |
| 30 | Cold start opens the top PM once, and only in the first sidebar | T, H | new |

### Supatrees

| # | Scenario | Layer | Status |
|---|---|---|---|
| 31 | `n` with one stack asks for a name; with several, asks for the stack first | T | ✓ |
| 32 | The name is validated as you type, including duplicates | T | ✓ |
| 33 | `d` on a tree confirms, then deletes it; the row goes, the cursor stays sensible | T | new |
| 34 | `s` syncs members and reports the result | T | new |
| 35 | `S` creates a stack | T | new |
| 36 | The active tree (this tab's) is marked; trees with unseen events are flagged until visited | T | ✓ |

### PR status (needs the seeded cache)

| # | Scenario | Layer | Status |
|---|---|---|---|
| 37 | Member badges for open / approved / changes requested / merged / closed | T | new |
| 38 | gh unauthenticated: hint shown, tick fetches stop, `r` retries | T | ✓ (logic; screen new) |
| 39 | gh rate-limited: hint shown, and it clears when the backoff ends | T | ✓ (logic) |

### Help, errors, messages

| # | Scenario | Layer | Status |
|---|---|---|---|
| 40 | `?` opens help; `j` / `k` / `G` scroll it; any other key closes | T | ✓ |
| 41 | Help folds under the description column at narrow widths (golden at 20 and 28) | T | ✓ (no golden) |
| 42 | An error shows at most 3 lines plus `e full error`; `e` opens the whole text, tabs expanded | T | ✓ |
| 43 | An async error arriving mid-prompt doesn't take over the prompt | T | new |
| 44 | Messages wider than the pane fold onto their own line above the keys | T | ✓ |

### Lifecycle and the real terminal

| # | Scenario | Layer | Status |
|---|---|---|---|
| 45 | `q` asks to confirm; the sidebar pane restarts supatree when it exits | H, E | new |
| 46 | `D` opens the dashboard tab, or focuses it if it is open | T, H | new |
| 47 | `supatree start` cold: session created, sidebar draws once input arrives | H, E | new |
| 48 | The sidebar survives zellij being slow or unavailable (circuit open): no hang, a hint | T | new |

## Harness notes (from step 1)

The harness is in `pkg/supatree/tui/harness_test.go`; the tests are in
`sidebar_teatest_test.go`.

- **Golden files hold the final model's `View()`, not teatest's
  `FinalOutput`.** The output stream is every frame with its cursor movement.
  It is right for "wait until X has appeared" (`waitFor`), and wrong for a
  golden file.
- **A test must not end with a command in flight.** A leftover command keeps
  calling zellij after the test's PATH is restored, so it reaches the real
  zellij, which fails. Three failures open the zellij package's process-wide
  circuit breaker, and then every zellij call is skipped for 60 seconds, in
  whichever tests run next. Leftover commands also write into temp dirs that
  are being deleted.

  `trackedModel` wraps the sidebar and tracks each command until `Update` has
  handled its result. `finish()` and the test cleanup wait for it to settle.
  The 30-second tick is told apart by age (over 250ms), since it only sleeps.
- **`finish()` settles before quitting,** so the final screen never depends on
  how fast a fake answered.
- **Your shell's `CLAUDE_CONFIG_DIR` is moved into the sandbox.** Opening an
  agent touches Claude's config.
- **Run under `-race` as well.** The tracker is shared between the program's
  goroutines.

## Order of work

1. ~~teatest dependency, `TestMain` colour profile, `fakeBin`, and the zellij
   and gh fakes. Port one existing test (help) to prove the harness.~~ Done,
   with scenario 17 (enter opens an agent's tab).
2. ~~Shared fixture builder matching `ux-env.sh`'s trees. Golden for scenario
   1.~~ Done (berlin and paris; oslo and the extra PM still to add).
3. The **new** agent and PM scenarios (17–19, 21, 23, 25–26, 29). These are
   where the bugs were.
4. Width goldens (2, 3, 5, 41), then the rest by section.
5. Seeded PR cache, shared by teatest and `ux-env.sh`. Scenarios 37–39, then a
   review tree in the sandbox.
