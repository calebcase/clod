# Random

## Purpose

Sandbox / scratch domain for clod smoke tests and ad-hoc commands.
Use it when you want a known-clean environment to verify that clod
itself is working — image builds succeed, the wrapper script starts
the agent, basic shell commands round-trip — without dragging in any
real-domain dependencies.

## Quick Start

### Prerequisites

Just docker. The image is `ubuntu:24.04`, so no GPU / SSH keys / API
tokens are needed for the *build* to succeed. (claude mode wants
authentication on first interactive run; crush mode pulls an Ollama
model on first run and works fully offline thereafter.)

### Workspace Setup

Nothing to set up — the domain is self-contained. Just `cd` here and
run `clod`.

## Available Tasks

This domain doesn't carry committed `TASK-*.md` files; it's meant for
ad-hoc requests. Typical smoke-test prompts:

- "Print the kernel version and the contents of /etc/os-release."
- "Write a 5-line haiku to ./haiku.txt."
- "Make a directory ./scratch/ and touch a file in it."

## Notes

- Used by `clod`'s authors to verify both tool drivers
  (`bin/clod-tool/claude.sh`, `bin/clod-tool/crush.sh`) still build
  and run after changes. See `testdata/README.md` at the repo root
  for the smoke-test commands.
- Anything written here is throwaway. Files left over from previous
  runs are fine; the agent should treat the dir as a fresh sandbox
  unless told otherwise.
