# How to use Claude session control (`cs`)

This is a how-to for running and controlling Claude Code sessions on
`experimentos`. For the full reference and an explanation of how `cs`
works inside, see [SESSIONS.md](SESSIONS.md).

## The idea in one paragraph

Each project gets its own **tmux session**, and the session is named after
the project. Claude Code runs inside it. tmux keeps it alive after you
disconnect (laptop closed, SSH dropped, phone app switched), so you can
reconnect later and carry on where you left off. An idle Claude still holds
about 150–450 MB of RAM. `cs` makes it one command to stop the ones you
aren't using and to resume them later. Resuming brings the conversation back,
because Claude saves it to disk.

## First time: get on the server

```bash
ssh kunchu01@100.118.33.89      # over Tailscale, from laptop or phone (Termius, Blink…)
cs                              # see what's there
```

`cs` on its own lists every project:

```
  NAME          STATE     IDLE     RAM  SINCE         DIR
  platform      no-tmux      -       -  -             ~/platform
  nammamentors  stopped     4h       -  -             ~/namma-mentors-cleanup
* platform2     running     0s    394M  Nov 14 10:33  ~
```

| STATE | Meaning | What to do |
|---|---|---|
| `running` | Claude is running in that tmux session | `cs attach <name>` |
| `stopped` | the tmux session exists but Claude has exited | `cs start <name>` |
| `no-tmux` | the project is configured and nothing is open | `cs start <name>` |
| `detached` | Claude was started outside tmux (e.g. straight from an SSH shell) | finish it there, or `cs stop --pid N` |

`*` marks the session you are in right now. IDLE is how long that session's
screen has gone without changing.

## Everyday recipes

### Start working on a project

```bash
cs start dalal          # opens tmux session "dalal" in ~/apps/dalal, runs claude --continue
cs attach dalal         # jump into it
```

- `--continue` reopens that folder's **last conversation**. To start a new
  one, use `cs start dalal --fresh`.
- If free RAM is below 1 GB, `cs` refuses to start. Stop something first,
  or add `--force`.
- You can start several projects at once (`cs start dalal ritual`) or all of
  them (`cs start --all`).

### Leave a session without stopping it

Inside tmux, press **`Ctrl-b`** and then **`d`** (detach). Claude keeps
running, and `cs attach dalal` brings you back later. Simply closing the SSH
window has the same effect.

### Switch between projects

From inside one session, `cs attach ritual` switches you to another. You
don't need to detach first. tmux's own picker also works: press `Ctrl-b` and
then `s`, choose with the arrow keys, and press Enter.

### Free up RAM

```bash
cs ram                     # who is using what, biggest first
cs stop dalal              # stop one (the tmux session stays; only claude exits)
cs stop --idle             # stop everything idle for 12 h or more
cs stop --idle 2h          # ...or pick your own threshold: 90m, 6h, 2d
cs stop --idle 2h --dry-run   # only show what would be stopped
cs stop --all              # stop every claude in tmux
```

`cs` will **never** stop the session you are typing in. Before stopping a
session, it prints that session's last few screen lines, so you can see what
was on it.

### Come back to a stopped project

```bash
cs start dalal && cs attach dalal
```

The same conversation comes back. Only text you typed in the prompt and
never sent is lost.

### Restart a stuck Claude

```bash
cs restart dalal           # stop + start, keeps the conversation
cs stop dalal --kill       # if it ignores the normal stop for 10 s, force it
```

### Add or remove a project

```bash
cs add blog ~/blog                     # name + folder
cs add notes ~/notes --model sonnet    # extra claude options after the folder
cs rm blog                             # removes it from the list (tmux session untouched)
cs edit                                # edit the list by hand
```

The list lives in `~/.config/cs/projects.conf`, one project per line:
`name | folder | extra claude args`. These projects are configured now:

| Name | Folder |
|---|---|
| platform | `~/platform` |
| nammamentors | `~/namma-mentors-cleanup` |
| dalal | `~/apps/dalal` |
| ritual | `~/apps/ritual` |
| focal | `~/focal` |
| dsa | `~/dsa-tracker` |
| home | `~` |

## A good daily routine

1. **Morning:** `cs` to see the list, then `cs start <today's project>` and
   `cs attach <it>`.
2. **Switching tasks:** `cs attach <other>`. There's no need to stop the
   first one if RAM is fine (look at `cs ram`, or the Memory panel in the
   Portal).
3. **End of day:** `cs stop --idle 2h`, which leaves anything you used
   recently running.

To let the server do step 3 by itself, run `cs prune-timer install`. It
then runs `cs stop --idle 24h` every day at 04:00. `cs prune-timer remove`
undoes it.

## tmux keys you'll actually need

`Ctrl-b` is the prefix. Press it, let go, then press the next key.

| Keys | What it does |
|---|---|
| `Ctrl-b` `d` | detach (leave it running) |
| `Ctrl-b` `s` | list sessions and switch |
| `Ctrl-b` `[` | scroll mode (arrows / PgUp; `q` to leave) |
| `Ctrl-b` `c` | new window in the same session (e.g. for a shell next to Claude) |
| `Ctrl-b` `n` / `p` | next / previous window |

## Good to know

- **After a reboot**, tmux-resurrect (in `~/.tmux.conf`) recreates the tmux
  sessions and re-runs `claude --continue` in the ones that had Claude
  running when they were last saved. Saves happen every 5 minutes. A
  session you had stopped stays stopped.
- **One project, one Claude.** If a project's Claude is already running,
  `cs start` just tells you so; attach to it instead.
- **Started Claude outside tmux?** It shows up as `pid-12345 detached`. It
  dies when that SSH connection closes. Next time, use `cs start`.
- **Something looks wrong?** `cs ram` shows each session's process tree.
  `~/platform/tools/cs-test.sh` runs the self-tests (about 17 s, and they
  don't touch your real sessions).

## Cheat sheet

```
cs                          list
cs start NAME [--fresh]     open + resume (or new conversation)
cs attach NAME              go in            Ctrl-b d = leave it running
cs stop NAME                stop one         cs stop --idle 2h = stop idle ones
cs restart NAME             stop + start
cs ram                      RAM per session
cs add NAME DIR | rm NAME   manage the project list
```
