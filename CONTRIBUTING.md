# Contributor guide

Working notes on how this project is developed. Short on purpose —
add sections as conventions emerge or get tripped over.

## Bumping the version

There are three places the version string lives: `README.md`
(human-facing badge), `bin/clod` (`CLOD_VERSION` for the bash
launcher's upgrade detection), and `bot/cli.go` (`Version` constant
the bot logs at startup). Keeping them in sync is mechanical —
drive it from the single source of truth in `VERSION`:

1. Edit `VERSION` to the new value (e.g. `0.32.0`).
2. Run `./update-version`. The script reads `VERSION` and rewrites
   the three downstream files in place.
3. Stage `VERSION` plus whatever the script changed, commit with
   the version in the subject (e.g. `v0.32.0: <one-line summary>`).

Bumping `bot/cli.go` (or any of the others) by hand is a known
foot-gun — the bot will report one version while the launcher
reports another, and the next install will silently re-init under
the wrong stamp. If you find yourself reaching for `sed`, you've
already lost; run the script.

What counts as a minor vs. patch bump follows
[Semantic Versioning](https://semver.org/): user-visible new
features or behavior changes go in minor, fixes in patch. The bot
and launcher share one version on purpose — they ship together.

## Commits

- Subject in the imperative mood, scoped: `bot: <change>`,
  `bin/clod: <change>`, `docs: <change>`.
- No `Co-Authored-By:` trailers. The git author is enough — these
  trailers cause noise across the user's repos.
- Body explains *why* and surfaces non-obvious context (the
  decision, the failure mode being prevented, the trade-off
  rejected). The diff explains *what*.

## Tests + docs ride along with behavior changes

If you change runtime behavior, update the tests that pinned the
old behavior and the docs that described it. The two places most
frequently lying are:

- `ARCHITECTURE.md` — section numbers track real subsystems; if
  you add or remove one, edit the section so the doc still maps
  to the code.
- `README.md` — features list near the top, env-var reference at
  the bottom of the bot section. Both go stale fast if you add
  or remove a knob.

Same goes for the resume / shutdown lifecycle: the on-disk
`SessionMapping` struct doc in `ARCHITECTURE.md` is consulted to
explain why a flag has the value it does. If you add a flag,
document it.
