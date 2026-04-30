# testdata/

Manual test fixtures for clod. Not automated — these exist so a person
(or claude, while driving the project) can spin up a known-good
workspace and exercise both supported tool drivers end-to-end after
making changes to `bin/clod` or the per-tool drivers in
`bin/clod-tool/`.

## workspace/

A minimal clod workspace. One domain, **random**, on `ubuntu:latest`.
Use it to verify that:

- `bin/clod` initialises a fresh `.clod/` correctly.
- The Dockerfile generated for whichever tool is selected actually
  builds (catches breakage like the Ollama-installer-hangs-on-systemd
  bug in v0.32.0).
- The container actually starts and the wrapper script works (claude
  prints its prompt; crush boots Ollama, pulls a model, drops into the
  TUI).

## Smoke-testing claude mode (default)

```bash
cd testdata/workspace/random
unset CLOD_TOOL                # ensure default
rm -f .clod/tool               # clear any persisted choice from a previous run
clod -p "echo hello from claude"
```

The first run takes a couple of minutes (image build + claude install).
Subsequent runs reuse the cached image.

## Smoke-testing crush mode

```bash
cd testdata/workspace/random
echo crush > .clod/tool
clod                           # interactive — Ctrl-C to exit
```

The first run takes longer than claude mode because it downloads
Ollama and the configured model. The model is auto-picked from the
host's largest GPU VRAM (see the README's *Switching the underlying
tool* section for the table); to keep iteration fast on a workstation
with lots of VRAM, override before the first run:

```bash
mkdir -p .clod/crush
echo 'qwen2.5-coder:0.5b' > .clod/crush/model
```

That pulls ~400 MB instead of 60+ GB.

## Switching back

`.clod/tool` is the source of truth between runs. Edit it (or
`rm` it to fall back to `claude`) before re-invoking `clod`. Anything
generated under `.clod/system/` will be regenerated on the next run
when the tool changes.

## Cleanup

The build artifacts under `.clod/system/`, the per-tool state
(`.clod/claude/`, `.clod/crush/`), and the container images are not
checked in. If you want to start completely fresh:

```bash
cd testdata/workspace/random
rm -rf .clod/system .clod/claude .clod/crush .clod/tool
docker rmi clod-random-* 2>/dev/null || true
```
