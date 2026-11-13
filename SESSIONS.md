# Claude Code sessions: `cs`

`cs` (`tools/cs`, symlinked as `~/.local/bin/cs`) manages the Claude Code CLI
sessions that run inside tmux on this server. Each project gets **one tmux
session with the same name as the project**. Inside it is an ordinary bash
shell, and `claude` runs in that shell.

Why it exists: an idle `claude` still holds 160–430 MiB of RAM. Six of them
used 1.6 GB. Claude Code saves every conversation to disk, so a session you
aren't using can be stopped and later resumed with `claude --continue`. `cs`
makes stopping and resuming one command each.

- Script: `~/platform/tools/cs` (bash, no other runtime needed; uses `tmux`, `ps`, `jq`)
- Config: `~/.config/cs/projects.conf` (outside the repo; the starting copy is `tools/cs.projects.example`)
- Tests: `~/platform/tools/cs-test.sh` (isolated: runs on a private tmux server with a fake claude)

## Commands

```bash
cs                       # = cs ls
cs ls                    # table: NAME STATE IDLE RAM SINCE DIR
cs ls --json             # same data as a JSON array (for the Portal later)
cs ram                   # running sessions by RAM, with their process trees

cs start dalal           # open tmux session "dalal" in ~/apps/dalal, run claude --continue
cs start dalal ritual    # several at once
cs start --all           # every project in the config
cs start focal --fresh   # start a NEW conversation instead of continuing
cs start dsa --force     # start even though free memory is below the threshold

cs stop dalal            # SIGTERM that claude; tmux session + shell stay
cs stop --idle           # stop everything idle >= 12h
cs stop --idle 90m       # ... or another threshold: 90m, 6h, 2d
cs stop --all            # every claude running in tmux
cs stop --pid 12345      # a claude started outside tmux (state "detached")
cs stop --idle 6h --dry-run   # only show what would be stopped
cs stop dalal --kill     # SIGKILL if it hasn't exited 10 s after SIGTERM

cs attach dalal          # attach, or switch-client if you are already in tmux
cs restart dalal         # stop + start (continues the conversation)
cs add blog ~/blog --model sonnet   # add a project (extra args optional)
cs rm blog               # remove from the config (the tmux session is left alone)
cs edit                  # open the config in $EDITOR
cs prune-timer status    # optional daily cleanup, see below
```

Example `cs ls`:

```
  NAME          STATE     IDLE     RAM  SINCE         DIR
  platform      no-tmux      -       -  -             ~/platform
  nammamentors  stopped    14m       -  -             ~/namma-mentors-cleanup
  dalal         no-tmux      -       -  -             ~/apps/dalal
* platform2     running     1s    451M  Nov 12 19:02  ~
1 claude session(s) running, 451 MiB total   (* = you are here)
other tmux sessions (no claude): bookingbot portal work
```

STATE values:

- `running`: a claude process lives somewhere in that tmux session.
- `stopped`: the tmux session exists, but no claude is running in it.
- `no-tmux`: the project is configured but has no tmux session.
- `detached`: a claude process running outside any tmux (listed as `pid-N` with its tty).

The table lists:

- every configured project;
- every other tmux session that runs claude (like `platform2`);
- detached claudes.

tmux sessions without claude that aren't projects only appear on the "other tmux sessions" line.

## How idle is measured

IDLE is the time since tmux last saw output in the **window where claude
runs** (`#{window_activity}`). For a stopped session it uses the most recent
activity across the session's windows. While Claude is working or you are
typing, its screen keeps changing, so IDLE stays near 0. A session nobody has
touched has a static screen, and IDLE grows. `cs stop --idle 12h` stops
sessions whose IDLE is 12h or more.

## Why stopping is safe (and what is lost)

- `cs stop` sends **SIGTERM**: a polite "please exit". Claude Code has already
  written the conversation to `~/.claude/projects/<dir>/<id>.jsonl`, and
  `claude --continue` in the same directory picks it up again. `cs start` does
  exactly that. It runs plain `claude` when that directory has no saved
  conversation yet.
- The tmux session and its shell **stay**. Only the claude process ends. You
  can attach and look around, or `cs start` it again.
- **What is lost:** text you typed in Claude's prompt but never sent. `cs`
  can't reliably detect that. Instead, before each stop it prints the last 3
  non-empty lines of the screen, so you can see what was there.
- It never stops **the session you are running it from**. It checks two things:
  1. the tmux session of `$TMUX_PANE`;
  2. whether one of `cs`'s own parent processes is a claude, which is the case
     when Claude runs `cs` as a tool. That claude and its tmux session are
     protected.

  You'll see `skipping <name>: this is the session you are in`.
- SIGKILL (cannot be caught, nothing gets saved cleanly) is only used with
  `--kill`, and only after SIGTERM has had 10 s (`CS_STOP_TIMEOUT`).
- `--all` and `--idle` only touch claudes inside tmux. A detached one has to be
  named explicitly with `--pid`, and `--pid` refuses any pid that isn't a
  claude process.

## Adding a project

```bash
cs add blog ~/blog                 # or: cs edit
```

The config has one project per line: `name | directory | extra claude args`.

```
# comments start with #
platform     | ~/platform                |
dalal        | ~/apps/dalal              | --model sonnet
notes        | ~/My Notes                |      # spaces in the directory are fine
```

- **name**: `[a-z0-9][a-z0-9_-]*`. It is also the tmux session name.
- **directory**: may start with `~`, and may contain spaces. It can't contain `|`.
- **extra args**: split on whitespace. No quotes, no `$VARS`. The file is read
  as data and never executed (`eval`).

## Optional: daily prune timer (not installed)

`cs prune-timer install` writes a **user** systemd timer and enables it:
`~/.config/systemd/user/cs-prune.{service,timer}`. No sudo is needed, and
linger is on, so it runs even when you're logged out. Every day at 04:00 it
runs `cs stop --idle 24h`. Its output (what it closed, and the last screen
lines) goes to the journal:

```bash
cs prune-timer install      # the one command to enable it
journalctl --user -u cs-prune.service     # what it did
cs prune-timer status       # next run
cs prune-timer remove       # undo
```

## Environment overrides

| Variable | Default | Purpose |
|---|---|---|
| `CS_CONFIG` | `~/.config/cs/projects.conf` | config file |
| `CS_CLAUDE_BIN` | `~/.local/bin/claude` | what `start` runs |
| `CS_MIN_FREE_MB` | `1024` | warn and require `--force` below this MemAvailable |
| `CS_STOP_TIMEOUT` | `10` | seconds to wait after SIGTERM |
| `CS_TMUX_SOCKET` / `cs -L name` | (default server) | another tmux server, like `tmux -L` |
| `CS_TMUX_CONF` | (none) | `tmux -f` file, used when cs starts a new server (tests use `/dev/null`) |
| `CS_CLAUDE_PROJECTS` | `~/.claude/projects` | where transcripts are looked for |

## How it works (for learning)

**tmux sessions vs the processes inside them.** A tmux *session* is just a
container held by the tmux server. It has windows, and windows have panes.
Each pane runs one process, here a bash shell (the "pane pid"). When you type
`claude` in that shell, claude becomes a **child** of the shell. Stopping
claude ends the child, and the shell (and so the session) carries on. That is
why `cs start` *types* the command into the shell with `tmux send-keys`
(`tools/cs:495`) instead of making claude the session's own command. With the
second approach, the session would vanish the moment claude exits. Before
typing, it sends `C-e C-u` (end of line, delete line) so any half-typed text
in that shell can't get glued onto the command. It only types into a pane
whose shell is idle at a prompt (`idle_shell_pane`, `tools/cs:424`). A new
session is created with `tmux new-session -d -s NAME -c DIR`
(`tools/cs:488`).

**Process trees and `/proc`.** Every process has a parent (PPID). `cs` reads
the whole table once with `ps -e -o pid=,ppid=,rss=,etimes=,tty=,comm=`
(`tools/cs:170-178`). That is the same data as `/proc/<pid>/stat`. It builds a
parent→children map, then walks down from each pane's shell to find claude
(`find_claudes`, `tools/cs:198`). It doesn't trust tmux's
`pane_current_command`, which may say `node` or `bash`. A process counts as
claude if its name is `claude`, or if it is `node` running
`.../claude-code/...` (`is_claude`, `tools/cs:185`). For protection it walks
**up** from its own pid (`find_protected`, `tools/cs:264`). A claude started
outside tmux has no `TMUX=` in `/proc/<pid>/environ` (`tools/cs:238`), and
that is how "detached" ones are told apart from ones in another tmux server.

**RSS.** Resident Set Size is the RAM pages a process currently holds. A
claude has children: tool shells, and MCP servers (often `node` or `python`).
So RAM is the RSS of claude **plus all its descendants** (`tree_rss`,
`tools/cs:214`), and `cs ram` breaks it down by command (`tree_summary`,
`tools/cs:221`). Shared libraries are counted once per process, so the sum
slightly overstates the real total. That's fine for spotting the big ones.

**SIGTERM vs SIGKILL.** SIGTERM (`kill -TERM`, `tools/cs:520`) asks a
process to exit, and the program can catch it and clean up. SIGKILL
(`tools/cs:527`) is handled by the kernel, so the program gets no say, and
anything unsaved is gone. `cs` waits for SIGTERM and only escalates when you
ask for it (`term_and_wait`, `tools/cs:518`).

**Where `--continue` looks.** Claude Code stores a project's conversations in
`~/.claude/projects/` under the directory path with every non-alphanumeric
character turned into `-`. For example, `/home/kunchu01/apps/dalal` becomes
`-home-kunchu01-apps-dalal` (`encode_dir`/`has_transcripts`,
`tools/cs:102-108`). If there is no `*.jsonl` there, `cs start` runs plain
`claude`.

**Bash habits used here.**

- `set -euo pipefail` (`tools/cs:25`) makes the script stop on errors and on
  unset variables.
- Commands are built as **arrays** and quoted (`"${cmd[@]}"`). The one line
  typed into tmux is escaped with `printf %q`.
- The config is parsed with `IFS='|' read` and `read -ra` (`tools/cs:143`,
  `tools/cs:477`), and never with `eval`.

## Tests

```bash
~/platform/tools/cs-test.sh     # ~17 s, prints ok/FAIL per check, exit 1 on any failure
```

The test runs against a private tmux server (`tmux -L cs-test-<pid>`, no
`~/.tmux.conf`), a temporary config, and a fake `claude` script that logs its
arguments and sleeps. It covers:

- start: new session, reused session, already running, `--fresh`, missing
  directory, bad name, low-memory guard;
- `ls`, `--json`, and `ram`;
- stop: by name, `--idle`, `--dry-run`, `--all`, `--pid`;
- protection of the current session (run from inside it) and of a claude
  ancestor;
- detached claudes, `restart`, `add`/`rm`, and directories with spaces.
