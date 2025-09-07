# clod <sub>*ˈkläd*</sub>

Run claude code in a modestly more secure way.

#### Install

Via user's home bin directory:

```bash
ln -s $(pwd)/bin/clod ~/bin/clod
```

If you don't have a home bin directory, you can make:

```bash
mkdir -p ~/bin
export PATH=$PATH:$HOME/bin
```

Add that export to your shell configuration (e.g. `.bashrc`).

#### Usage

```bash
cd <directory>
clod
```

If you had a previous claude config in your home directory a copy of it would
have been used to initialize the directory. Otherwise a new config will be
created. After the session is ended you can save the claude config and reuse
it:

```bash
cp .clod/claude/claude.json ~/.claude.json
```

Now when a new directory is initialized it will reuse that base config. Put the
settings you want used globally in this config.

Once initialized the `.clod` directory has the configuration files and they can
be modified if necessary. For example, if you want to install additional
packages edit `.clod/Dockerfile` and add a new `RUN`. After you are done
editing run `.clod/build` to rebuild the docker image.

If you need to adjust the docker run command (e.g. adding a port forward or
additional mount), edit `.clod/run`.

If this containment mechanism is sufficent for you, then you can also reduce
the burden of claude asking for permissions to do things, either in the claude
config, or [command line flags][claude-permission-modes]:

```
clod --permission-mode acceptEdits
```

Or even disable them entirely if you don't mind it having [unfettered access to
the internet and such][claude-dangerously-skip-permissions]:

```
clod --dangerously-skip-permissions
```

clod plumbs the flags directly to claude:

```
$ clod --help
Claude Code - starts an interactive session by default, use -p/--print for non-interactive output

Arguments:
  prompt                                            Your prompt

Options:
  -d, --debug [filter]                              Enable debug mode with optional category filtering (e.g., "api,hooks" or
                                                    "!statsig,!file")
  --verbose                                         Override verbose mode setting from config
...
```

#### Similar Projects

There are several similar projects. If the design choices for clod aren't to
your liking possibly one of these will be:

* https://docs.anthropic.com/en/docs/claude-code/devcontainer
* https://github.com/RchGrav/claudebox
* https://github.com/VishalJ99/claude-docker

---

[claude-permission-modes]: https://docs.anthropic.com/en/docs/claude-code/iam#permission-modes
[claude-dangerously-skip-permissions]: https://docs.anthropic.com/en/docs/claude-code/devcontainer
