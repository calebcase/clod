# clod <sub>*ˈkläd*</sub>

Run claude code in a modestly more secure way.

#### Install

Via user's home bin directory:

```bash
ln -s $(pwd)/bin/clod ~/bin/clod
```

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
cp .clod/config/claude.json ~/claude.json
```

Now when a new directory is initialized it will reuse that base config. Put the
settings you want used globally in this config.

Once initialized the `.clod` directory has the configuration files and they can
be modified if necessary. For example, if you want to install additional
packages edit `.clod/Dockerfile` and add a new `RUN`.
