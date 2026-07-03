# v1 — Bot-Owned Scheduling MCP

**Status:** draft, expect iteration
**Owner:** caleb
**Related:** eagle 8-hour dead-zone incident (2026-07); prior MCP work in `bot/permbridge/`, `bot/permission.go`

## 1. Problem

Claude-code exposes scheduling primitives — `CronCreate`/`CronList`/`CronDelete`, `ScheduleWakeup`, `/loop`, `Bash run_in_background=true` + `Monitor`/`BashOutput` — that our per-turn harness silently breaks in two ways:

1. **Container/process restart** drops all in-container state. Every `@bot set model=…`, graceful shutdown, or discuss injection cycles the docker container.
2. **Session event loop stops** the moment claude ends a turn with no outstanding tool call. Session-scoped schedulers go dormant even without a restart.

Concrete failure: eagle scheduled a 1-minute watchdog cron via `CronCreate` on 2026-07-02. Claude ended a turn cleanly reporting progress; the cron went dormant; ~8 hours of GPU-idle time went unnoticed until the user @-mentioned. Claude self-diagnosed: *"per `CronList`, the cron is session-only. It only fires while an agent session is actively looping."*

We've already updated `bot/clod_runtime_prompt.txt` to warn the model off these features and point at `nohup` + state-file workflows instead. That's a workaround, not a fix. This document proposes the fix: **the bot itself owns the scheduler and exposes it to the agent as an MCP server**, so cron and background jobs work correctly regardless of container lifecycle or turn-end semantics — and portably across agents (claude-code today, Crush tomorrow, both speak MCP).

## 2. Goals & non-goals

**Goals**

- Durable interval work: scheduled ticks fire on wall-clock time, survive container restarts, survive turn-end-waiting.
- Durable background processes: long-running host-side commands that survive container restarts.
- Transparent to the model: tools present as MCP entries alongside the built-in ones. The model doesn't have to know it's talking to the bot vs. Anthropic.
- Portable across agent backends: same MCP server plugs into claude-code (`--mcp-config`) and into Crush (`crush.json`).
- Additive: existing bot behavior unchanged unless the model calls a new tool.

**Non-goals (v1)**

- Full claude-code `CronCreate` feature parity. No `@-mention` aliases, no `WAIT_FOR_PROMPT_INSTRUCTIONS`, no per-user rate-limit tiers. Just cron + bg.
- Cross-thread scheduling. A cron is owned by one Slack thread (session). No "global" schedules.
- Web/UI for cron management. Home tab visibility is a phase-4 stretch, not v1.
- Zulip/Matrix support. Chat abstraction is a separate track.
- Replacing the permission MCP. Keep concerns split (see §4.1).

## 3. Architecture

```
                        ┌──────────────────┐
                        │  Slack (or later │
                        │    Zulip, etc.)  │
                        └────────┬─────────┘
                                 │
                                 ▼
                        ┌──────────────────┐
                        │    bot process   │◄─── goroutines:
                        │  (host, 24/7 up) │      • cron ticker
                        │                  │      • bg process supervisor
                        │  sessions.json   │
                        │  crons.json ◄────┼──── new persisted state
                        │  bgjobs.json     │
                        └────┬─────────────┘
                             │
                             │  FIFO pair per runtime dir
                             │  (existing pattern from permbridge)
                             │
        ┌────────────────────┼────────────────────┐
        │                    │                    │
        ▼                    ▼                    ▼
┌─────────────┐      ┌─────────────┐      ┌─────────────┐
│  container  │      │  container  │      │  container  │
│  (eagle)    │      │  (falcon)   │      │  (jay)      │
│             │      │             │      │             │
│  permbridge │      │  permbridge │      │  permbridge │
│  schedbridge│◄─────┤ new binary  │      │             │
│             │      │             │      │             │
│  claude     │      │  claude     │      │  claude     │
└─────────────┘      └─────────────┘      └─────────────┘

     agent sees MCP tools:
       - request_permission     (existing)
       - cron_create / _list / _delete   (new)
       - bg_run / _output / _status / _kill / _list  (new)
```

Every existing invariant carries over unchanged: one bot process on the host, one docker container per session, one MCP bridge binary per container writing over FIFOs to the bot. The scheduler goroutines run inside the bot, where they survive container restarts by construction.

## 4. Design decisions (open — this is what we're iterating on)

### 4.1 One binary or two? — `permbridge` + `schedbridge`, or extend `permbridge`?

**Proposal:** two binaries — `permbridge` stays as is, new `schedbridge` handles cron+bg tools. Both embedded into the bot, both written into each runtime dir, both listed in the generated `mcp_config.json`.

**Rationale:** they have different failure characteristics. Permission is synchronous request/response, one-shot per model action, no state. Scheduling is asynchronous, stateful, involves the bot's ticker goroutines and process supervisor. Bundling them means a bug in the scheduler paths can wedge the permission path. Separate binaries also let us iterate on `schedbridge` (its wire format, its FIFO semantics) without risking regressions in the permission flow. Cost: one more embedded binary in the bot (~2MB), one more MCP entry in the config.

**Alternative:** one binary. Marginally simpler runtime dir. Marginally worse concern separation. **Recommend against.**

**Open question 1:** two binaries or one?

### 4.2 FIFO transport or Unix socket?

Permbridge uses two FIFOs (request from container → bot, response from bot → container). Works fine for one-shot RPC. Scheduling has an async event class the FIFO pattern doesn't handle well: **the bot needs to notify the container when a cron tick fires** (poke mode, §4.5). A FIFO pair is one-way each; adding a third FIFO for bot-initiated events works but is awkward.

**Proposal:** Unix domain socket for `schedbridge`. Bidirectional, JSON-lines framing, keeps the "no extra runtime dependencies in the container" invariant (still just a compiled Go binary, no python etc.). The socket lives in the runtime dir alongside the existing FIFOs.

**Cost:** slightly more Go boilerplate on both sides. But it gives us clean bidirectional messaging for free, which we'll need for poke mode and for real-time bg output streaming if we want it later.

**Open question 2:** FIFO trio or Unix socket?

### 4.3 State persistence — sessions.json or dedicated files?

**Proposal:** dedicated files. `crons.json` and `bgjobs.json` next to `sessions.json` at the bot's storage root. Rationale: `sessions.json` is already heavy (all thread state); adding array fields for crons and bg jobs balloons it and complicates the lock. Dedicated files let each subsystem save independently and make disk-inspection easier during incidents.

**Schema sketch** (deliberately minimal for v1):

```json
// crons.json
{
  "crons": [
    {
      "id": "cron_a1b2c3d4",
      "session_key": "D08FDQEC7GB:1781738874.032559",
      "spec": "@every 5m",
      "mode": "command",
      "command": "cd /home/…/eerie-eagle && python watchdog.py",
      "cwd": "/home/…/eerie-eagle",
      "description": "watchdog: check R16 refine progress",
      "created_at": "2026-07-03T10:15:00Z",
      "last_fire_at": "2026-07-03T11:20:00Z",
      "last_exit_code": 0,
      "last_output_path": "/home/…/eerie-eagle/.clod/schedbridge/cron_a1b2c3d4.log",
      "next_fire_at": "2026-07-03T11:25:00Z"
    }
  ]
}
```

```json
// bgjobs.json
{
  "jobs": [
    {
      "id": "bg_e5f6g7h8",
      "session_key": "D08FDQEC7GB:1781738874.032559",
      "cmd": "python train.py --config r16_refine.yaml",
      "cwd": "/home/…/eerie-eagle/inference",
      "description": "R16 refine training run",
      "pid": 3984999,
      "started_at": "2026-07-02T14:30:00Z",
      "log_path": "/home/…/eerie-eagle/.clod/schedbridge/bg_e5f6g7h8.log",
      "status": "running",
      "exit_code": null,
      "ended_at": null
    }
  ]
}
```

**Open question 3:** dedicated files or extend `sessions.json`?

### 4.4 Execution locus — inside or outside the container?

For cron `command` mode and `bg_run`: the command claude issues could execute inside the container (via docker exec into the running session's container) or on the host (as the bot user, cwd set to the domain dir).

**Proposal:** on the host, as the bot user, cwd pinned to the session's domain dir. Rationale:

- Survives container restart by design. Inside-container execution dies with the container, which is exactly the failure mode we're fixing.
- Eagle-style workflows already spawn Python jobs that run on the host with GPU access — `bg_run` matches how the user runs work today.
- Simpler than orchestrating `docker exec` and dealing with per-container tty/network state.

**Security constraint:** the bot must reject commands whose resolved cwd escapes the session's domain dir. Since claude's harness runs inside a container that only has the domain dir mounted, its natural cwd sense doesn't escape by default — but the raw command string is under model control. Bot-side check: resolve the requested `cwd` to a real path, verify it's a subdirectory of the session's domain path, reject otherwise. Command arguments are not sandboxed further — same trust model as the permission-approved Bash tool today, which is `bypassPermissions` mode.

**Open question 4:** host execution (proposal) or docker exec into the container? Related: should `bg_run` support both, with a flag?

### 4.5 Cron modes — command vs poke

Two tick behaviors:

- **`mode: "command"`** (default): the bot runs the pre-declared shell command on the host, appends stdout+stderr to the cron's log file, records exit code. Claude sees results next time it reads the log or calls `cron_list` (which reports `last_exit_code`, `last_fire_at`, and `last_output_path`). Cheap — no agent turn per tick.
- **`mode: "poke"`**: the bot injects a synthetic user message into the thread's claude stdin: `"[cron:<description>] tick at <ts>"`. Starts a fresh claude turn. Expensive (one turn per tick, tokens billed) but claude reasons about what to do. Analogous to how a human might @-mention "any update?"

**v1 recommendation:** ship `command` mode first. Poke mode has more edge cases (what if a turn is in flight? what if the previous tick's turn hasn't ended? what if the user is mid-conversation?). Command mode covers the eagle failure — watchdog just needs to run and record; claude reads the record next time it's active. Add poke in phase 3 once we've validated the command path.

**Open question 5:** command-only for v1, or ship both?

### 4.6 Cron spec syntax

**Proposal:** support both standard cron format (`* * * * *`) and `github.com/robfig/cron/v3`'s shorthand descriptors (`@every 5m`, `@hourly`, `@daily`, `@midnight`). The robfig library is battle-tested and small (~1500 LOC).

**Minimum interval:** 30 seconds. Anything shorter is either a poll (should use `nohup` + a loop) or a bug.

**Maximum crons per session:** 16. Tunable via a bot flag, but keep the default small so runaway `cron_create` loops can't accumulate. Ties into the fact that these are per-thread.

### 4.7 Tool naming

Two conventions in play:
- Anthropic uses snake_case for MCP tool names (`request_permission`).
- Claude-code's built-in tools use PascalCase (`CronCreate`, `BashOutput`).

**Proposal:** snake_case — `cron_create`, `cron_list`, `cron_delete`, `bg_run`, `bg_output`, `bg_status`, `bg_kill`, `bg_list`. Matches the existing `request_permission` tool and MCP convention. The model will discover them via MCP list-tools and call them by whatever names we choose; it doesn't matter that they don't look like `CronCreate`.

**Open question 6:** are we sure snake_case is right? Should we mirror claude-code naming to make it "feel" like the native tools claude already knows?

### 4.8 Session close semantics

**Proposal:** closing a session (`@bot close`, or session archival) does the following:

- Cancel all active crons for that session.
- Send SIGTERM to all bg jobs, give them 10 seconds, then SIGKILL.
- Persist final state so `cron_list` / `bg_list` show `status: "cancelled"` if the session is later re-opened.

Rationale: crons and bg jobs are session-scoped. A closed thread means the user has moved on; leaving processes and schedules running is a resource leak and a UX surprise. If the user really wants a cron to outlive the session, they can register it externally (host cron/systemd) — which is out of scope for this design.

**Open question 7:** aggressive cleanup (proposal) or preserve across close so a `@bot resume` picks them up?

## 5. MCP tool schemas

Written as JSON Schema fragments; final wire format is standard MCP `tools/list` and `tools/call` per the spec.

### 5.1 `cron_create`

```json
{
  "name": "cron_create",
  "description": "Schedule a shell command to run on a recurring interval, driven by the bot on the host. Survives container restarts and turn-end-waiting. For interval work that must keep advancing regardless of whether the agent is actively responding.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "spec":        {"type": "string", "description": "Cron expression (5-field) or shorthand: '@every 5m', '@hourly', '@daily'. Minimum interval 30s."},
      "command":     {"type": "string", "description": "Shell command to execute at each tick. Runs via `bash -c` on the host as the bot user."},
      "cwd":         {"type": "string", "description": "Working directory. Must be under the session's domain dir. Defaults to the domain dir."},
      "description": {"type": "string", "description": "Human-readable label for logs and `cron_list` output."},
      "mode":        {"type": "string", "enum": ["command"], "description": "v1 supports 'command' only (bot runs the command; agent reads results). 'poke' (inject a turn) is planned for a later phase."}
    },
    "required": ["spec", "command", "description"]
  }
}
```

Returns: `{ "id": "cron_<8-hex>", "next_fire_at": "<iso8601>" }`.

### 5.2 `cron_list`

Returns the current cron array for this session, including `last_fire_at`, `last_exit_code`, `last_output_path`, `next_fire_at`.

### 5.3 `cron_delete`

Input: `{ "id": "cron_<8-hex>" }`. Returns `{ "deleted": true }` or an error if not found.

### 5.4 `bg_run`

```json
{
  "name": "bg_run",
  "description": "Start a long-running shell command in the background on the host, outside the docker container. The process survives container restarts. Stdout+stderr are captured to a log file. Use bg_output to read progress, bg_status to check liveness, bg_kill to stop.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "cmd":         {"type": "string"},
      "cwd":         {"type": "string"},
      "description": {"type": "string"}
    },
    "required": ["cmd", "description"]
  }
}
```

Returns: `{ "id": "bg_<8-hex>", "pid": 12345, "log_path": "/…/.clod/schedbridge/bg_<id>.log" }`.

### 5.5 `bg_output`

Input: `{ "id": "bg_<8-hex>", "tail_bytes": 8192 }` (default 8KB). Returns the last N bytes of the log file.

### 5.6 `bg_status`

Input: `{ "id": "bg_<8-hex>" }`. Returns `{ "status": "running|exited|killed|orphaned", "exit_code": …, "started_at": …, "ended_at": … }`. `orphaned` means the recorded PID is no longer alive but we don't have a recorded exit (bot restarted while it was running; process may or may not still be present under that PID).

### 5.7 `bg_kill`

Input: `{ "id": "bg_<8-hex>", "signal": "TERM|KILL" }`. Default TERM. Returns `{ "killed": true, "sent_signal": … }`.

### 5.8 `bg_list`

Returns the current bg-jobs array for this session.

## 6. Runtime prompt update

The `bot/clod_runtime_prompt.txt` guidance already tells claude not to use `CronCreate`/`ScheduleWakeup`/`Bash run_in_background`. Once the shim ships, replace the "don't use these, do this" section with:

> This runtime provides its own scheduling and background-job tools via MCP: `cron_create` / `cron_list` / `cron_delete` for interval work, `bg_run` / `bg_output` / `bg_status` / `bg_kill` / `bg_list` for long-running host-side processes. Prefer these over Claude-native `CronCreate` / `ScheduleWakeup` / `Bash run_in_background=true`, which do not survive container restarts or turn-end-waiting in this harness.

Home tab help gains a paragraph documenting the two tool families for user awareness.

## 7. Bot lifecycle interactions

**Bot startup:**
1. Load `crons.json`, `bgjobs.json`.
2. For each cron: start a robfig cron entry with its stored spec.
3. For each bg job with `status: "running"`: check whether the PID still exists (`kill -0`). If yes, resume tracking. If no, mark `status: "orphaned"` and persist. (We can't tell after the fact whether it exited cleanly or was killed; the log file usually tells us via its tail.)
4. Start the socket listener that accepts connections from per-container `schedbridge` binaries.

**Bot shutdown:**
1. Stop cron scheduler.
2. Do NOT kill bg jobs — the user's work should survive bot restarts. Just persist current state and disconnect.
3. On next startup, resume tracking (per bullet 3 above).

**Session start:**
1. `schedbridge` binary is written into the runtime dir alongside `permbridge` (both from embedded copies).
2. Runtime `mcp_config.json` includes both entries.
3. `schedbridge` connects to the bot's socket, registers its session_key.

**Session close (`@bot close`):**
1. Cancel all crons owned by this session.
2. SIGTERM bg jobs, wait 10s, SIGKILL. Persist final state.
3. Runtime dir is torn down normally.

**Container restart mid-session (e.g. `@bot set model=`):**
1. Old container dies. `schedbridge` connection drops; bot logs it and marks the session's bridge as disconnected.
2. Crons and bg jobs keep running — they're bot-owned, not container-owned.
3. New container starts. New `schedbridge` reconnects. Bot re-registers.
4. Next `cron_list` / `bg_list` from claude shows everything is still there.

## 8. Failure modes and edge cases

- **Command exits non-zero on every tick.** We record `last_exit_code`, keep firing. Claude discovers via `cron_list` and can `cron_delete`.
- **Command hangs longer than the cron interval.** Skip overlapping ticks; log a warning. Alternative: allow concurrent instances (config flag). v1: skip overlap.
- **bg job dies unexpectedly.** Status transitions `running → exited` with the actual exit code, `ended_at` set. If we missed the exit (bot was down), status becomes `orphaned`.
- **Bot crashes and restarts.** Crons resume from spec (may miss the ticks that would have fired during downtime — no catch-up). bg jobs resume from PID probe. Both persisted state files are single-write-then-rename to survive crash mid-write.
- **Model creates 100 crons.** Cap at 16 per session; return an error on the 17th `cron_create`. The cap is per-session, not global.
- **Schedule fires while previous tick's command still running (command mode) and overlap disabled.** Skip; log; continue.
- **User closes session while bg job is doing important work.** SIGTERM warning message posted to the thread before we send the signal, giving the user a `~10s window to cancel the close`? Or just kill without warning? *v1: kill without warning; the user just typed `close`, that's the signal.* Reconsider if pain point.

## 9. Test plan

**Unit:**
- Cron spec parsing (valid and invalid).
- ID generation uniqueness.
- State-file roundtrip (persist → reload → compare).
- Path-under-domain-dir validation for `cwd`.

**Integration:**
- Create cron with `@every 30s`, verify tick fires ~30s later, verify log file grows.
- Restart bot, verify cron is still firing after startup.
- Restart container (simulate `set model=`), verify cron is unaffected.
- `bg_run` a `sleep 300`, restart bot, verify status is still `running` on reload.
- `cron_create` up to the cap, verify 17th fails with a clear error.

**E2E:**
- From within a claude session, call `cron_create` for a 1-min watchdog that appends to a file. Wait 3 minutes. Verify 2-3 ticks in the file, `cron_list` shows expected counts.
- Simulate the eagle failure: create the watchdog, have claude end its turn, wait 15 minutes without touching Slack. Confirm ticks continued firing throughout.

## 10. Implementation phases

**Phase 1** — Scheduling MCP plumbing + `cron_*` command mode.
- New `bot/schedbridge/main.go` binary, embedded into the bot the same way as permbridge.
- Bot-side socket listener + `SchedulingRegistry` in Go.
- Persistence to `crons.json`.
- `cron_create` / `cron_list` / `cron_delete` end-to-end.
- Runtime prompt update advertising the new tools.
- Test cases per §9.
- Ship. Version bump 0.36.0.

**Phase 2** — `bg_*` tools.
- Host-side process supervisor in the bot.
- `bgjobs.json` persistence.
- `bg_run` / `bg_output` / `bg_status` / `bg_kill` / `bg_list`.
- PID-probe reconciliation on bot startup.
- Test cases including bot-restart-mid-job.

**Phase 3** — Cron poke mode.
- `mode: "poke"` on `cron_create` — bot injects a synthetic user message into the session's claude stdin at each tick.
- Coalescing: skip a tick if a turn is currently in-flight; optionally queue with a max-depth of 1.
- Rate limiting.

**Phase 4** — UX polish.
- Home tab section: "Active schedules and jobs across your sessions."
- `@bot cron list` / `@bot bg list` commands for direct inspection without an agent turn.
- Slack notice when a cron fires with non-zero exit or a bg job crashes.
- Optional: emoji indicator on the session anchor when there are active crons/bg jobs.

## 11. What we're deliberately deferring

- Claude's native `CronCreate` compatibility shim (e.g., intercept `CronCreate` calls and route to `cron_create`). If we do this, it's a follow-up after v1 has settled.
- Crush integration. The MCP config format is different (`crush.json` `mcp` section vs claude-code's `--mcp-config` file), but the wire protocol is identical. Documented as a "yes this will work" but not shipped in v1.
- Global crons (bot-wide, not session-scoped). Would need a separate registration path (via `@bot` command) since there's no session to own them.
- Cron output posting to the Slack thread. Tempting, but noisy — `cron_list` reads log paths on demand is fine for v1.
- Multi-command chains (`cron_create` with a sequence of commands). YAGNI; `bash -c 'cmd1 && cmd2'` covers this.

## 12. Open questions summary (rule on these before implementation starts)

1. Two binaries (`permbridge` + `schedbridge`) or extend `permbridge`?
2. FIFO trio or Unix socket for `schedbridge`?
3. Dedicated `crons.json` + `bgjobs.json`, or extend `sessions.json`?
4. Host execution or `docker exec` into the container for cron commands and bg jobs?
5. Ship `command` mode only in v1, or both `command` and `poke`?
6. Snake_case tool names (`cron_create`) or PascalCase (`CronCreate`) to mirror claude-code?
7. Session close: aggressive cleanup (proposal) or preserve for later resume?
8. Concurrent-tick behavior when a command mode cron overlaps with itself: skip (proposal), queue, or run concurrently?
9. Should there be a Slack-side notification when a cron fires with a non-zero exit code, or is that left for the model to discover via `cron_list`?
10. Maximum crons per session default of 16 — right ballpark?

## 13. Not answered yet

- Exact JSON-RPC framing on the socket (probably JSON-lines with length prefix, but confirm during phase 1).
- How the `schedbridge` binary discovers the socket path. Simplest: bot writes it to an env var (`SCHEDBRIDGE_SOCKET`) or a well-known file in the runtime dir when it spawns the container.
- Whether we should embed the go cron library or roll our own tiny scheduler. robfig/cron is 4KB minified deps but adds a dependency; a hand-rolled `@every` parser is <100 lines of code. Probably robfig for real cron expressions, hand-rolled for `@every` if we want zero deps.
- What "session_key" means in a world where the same domain has multiple concurrent threads. Currently the bot keys by `channel:thread_ts` — reuse that verbatim.
