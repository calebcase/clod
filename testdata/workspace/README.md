# clod test workspace

Minimal workspace for exercising clod end-to-end. One domain, in
`random/`, intended for sandbox-style "run a quick command" work. The
README files in this workspace are deliberately short — clod inlines
them as system-prompt context for whatever agent is running, and we
don't want the test fixture to bias actual coding tasks elsewhere.

## Conventions

- One domain (`random/`).
- Image: `ubuntu:24.04` (latest LTS — small, predictable, good for
  smoke tests).
- No project dependencies in `Dockerfile_project`; the random domain
  is meant to run trivial commands. If you need extra packages for a
  test, add them under `random/.clod/Dockerfile_project` and re-run
  clod (the change will trigger a rebuild via the standard
  `changes_detected` path).

## When you need user input

Use the `AskUserQuestion` tool. Don't end a turn with a question in
prose — the bot's idle-skip logic relies on "no outstanding tool call
at end of turn = task complete" to avoid waking dormant threads on
restart.
