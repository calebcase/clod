package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/slack-go/slack"
)

// initImageOption is one suggested base image for a new `.clod` setup.
type initImageOption struct {
	Value       string // goes into .clod/image
	Label       string
	Description string
}

// initImageOptions is the curated list surfaced in the init prompt.
var initImageOptions = []initImageOption{
	{"ubuntu:24.04", "ubuntu:24.04 (default)", "General-purpose Linux base. Good for most tasks."},
	{"golang:latest", "golang:latest", "Go toolchain preinstalled."},
	{"node:20", "node:20", "Node.js 20 with npm."},
	{"python:3.12", "python:3.12", "Python 3.12 with pip."},
	{"nvidia/cuda:12.3.0-devel-ubuntu22.04", "nvidia/cuda:12.3.0", "CUDA-enabled for ML/GPU tasks."},
}

// initSSHOption is one SSH forwarding mode for `.clod/ssh`.
type initSSHOption struct {
	Value       string
	Label       string
	Description string
}

var initSSHOptions = []initSSHOption{
	{"auto", "auto (recommended)", "Use an existing SSH agent or spawn one on demand."},
	{"false", "false", "No SSH forwarding into the container."},
	{"true", "true", "Require a pre-existing SSH agent; fail if none."},
}

// initModelOption is one model choice for `claude --model`. Values must
// match what the model-switch reaction emoji mapping uses so that the
// indicator reaction ends up consistent after setup.
type initModelOption struct {
	Value       string
	Label       string
	Description string
}

var initModelOptions = []initModelOption{
	{"opus", "🎼 Opus (most capable)", "Best for complex reasoning and hard problems. Slower and more expensive."},
	{"sonnet", "📜 Sonnet (balanced, recommended)", "Good default for most tasks. Faster and cheaper than Opus."},
	{"claude-haiku-4-5", "🌸 Haiku (fast and cheap)", "Best for quick, simple tasks. Fastest and cheapest."},
}

// defaultAptPackages are always offered in the package picker.
var defaultAptPackages = []string{
	"ca-certificates",
	"curl",
	"ffmpeg",
	"file",
	"git",
	"imagemagick",
	"jq",
	"python3-venv",
	"ripgrep",
	"unzip",
}

// dockerfileAptPattern pulls individual package names out of a
// `Dockerfile_project`-style apt-get install block. Matches lines that look
// like indented bare identifiers inside a RUN block.
var dockerfileAptPattern = regexp.MustCompile(`(?m)^\s+([a-zA-Z0-9][a-zA-Z0-9.+_-]{1,})$`)

// discoverSiblingPackages walks sibling task directories under basePath and
// extracts apt package names from their `.clod/Dockerfile_project` files.
// Returns packages that appear in two or more siblings, sorted.
func discoverSiblingPackages(basePath, excludeTask string) []string {
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return nil
	}
	counts := map[string]int{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Name() == excludeTask {
			continue
		}
		path := filepath.Join(basePath, e.Name(), ".clod", "Dockerfile_project")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		// Only look at the apt install block. Everything below the first
		// `apt-get install` up to the end of the RUN continuation.
		text := string(data)
		idx := strings.Index(text, "apt-get install")
		if idx == -1 {
			continue
		}
		block := text[idx:]
		// Stop at the first non-continuation blank-ish line after the block.
		if end := strings.Index(block, "\nRUN "); end != -1 {
			block = block[:end]
		}
		seen := map[string]bool{}
		for _, m := range dockerfileAptPattern.FindAllStringSubmatch(block, -1) {
			name := m[1]
			// Skip obvious noise (flags, variables, lone-word command names
			// that aren't packages).
			if strings.HasPrefix(name, "-") || strings.Contains(name, "=") {
				continue
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			counts[name]++
		}
	}
	var out []string
	for pkg, n := range counts {
		if n >= 2 {
			out = append(out, pkg)
		}
	}
	sort.Strings(out)
	return out
}

// discoverTemplateTasks lists sibling directory names under basePath
// (excluding the new task's own name) that are candidates for copying as
// a template. We don't require a `.clod/` to exist — any sibling directory
// with content is a valid starting point — but we do skip dotfiles so
// bot bookkeeping (.git, .DS_Store, etc.) doesn't show up as a choice.
func discoverTemplateTasks(basePath, excludeTask string) []string {
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == excludeTask || strings.HasPrefix(name, ".") {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// initPackageSuggestions builds the deduplicated package list for the
// checkbox picker: defaults + packages appearing in 2+ sibling tasks.
func initPackageSuggestions(basePath, excludeTask string) []string {
	seen := map[string]bool{}
	var out []string
	for _, pkg := range defaultAptPackages {
		if seen[pkg] {
			continue
		}
		seen[pkg] = true
		out = append(out, pkg)
	}
	for _, pkg := range discoverSiblingPackages(basePath, excludeTask) {
		if seen[pkg] {
			continue
		}
		seen[pkg] = true
		out = append(out, pkg)
	}
	sort.Strings(out)
	return out
}

// pendingInit is the server-side state for an outstanding init prompt. Stored
// in handler.pendingInits keyed by the Slack thread key so action-button
// clicks can find it (Slack action values have a 2000-char cap).
type pendingInit struct {
	MessageTS    string
	ChannelID    string
	ThreadTS     string
	TaskName     string
	TaskPath     string
	CreateDir    bool // true = directory doesn't exist; false = exists but uninitialized
	Instructions string
	UserID       string
	MentionTS    string   // ev.TimeStamp of the user's @-mention
	Packages     []string // suggestion list, also the checkbox options
	Templates    []string // sibling task names available as templates (empty for none)
	SelImage     string   // currently-selected image value
	SelSSH       string   // currently-selected ssh mode
	SelModel     string   // currently-selected model (value from initModelOptions)
	SelTemplate  string   // currently-selected template (sibling task name) or "" for none
	SelPackages  []string // currently-selected package indices (as strings)
}

// buildInitPromptBlocks renders the setup prompt: header, image radio,
// ssh radio, package checkboxes, Create/Cancel buttons.
func buildInitPromptBlocks(p *pendingInit, progressKey string) []slack.Block {
	var headerText string
	if p.CreateDir {
		headerText = fmt.Sprintf(":sparkles: *Set up new task* `%s`\n_The directory `%s` doesn't exist yet. I'll create it and initialize a default `.clod/` setup._", p.TaskName, p.TaskPath)
	} else {
		headerText = fmt.Sprintf(":sparkles: *Initialize task* `%s`\n_The directory exists but isn't set up for clod. I'll create `.clod/` with the options below._", p.TaskName)
	}

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", headerText, false, false),
			nil, nil,
		),
	}

	// Base image (radio).
	imageOpts := make([]*slack.OptionBlockObject, 0, len(initImageOptions))
	var initialImage *slack.OptionBlockObject
	for _, o := range initImageOptions {
		label := truncateForSlackText(o.Label, 75)
		desc := truncateForSlackText(o.Description, 75)
		opt := slack.NewOptionBlockObject(
			o.Value,
			slack.NewTextBlockObject("plain_text", label, false, false),
			slack.NewTextBlockObject("plain_text", desc, false, false),
		)
		imageOpts = append(imageOpts, opt)
		if o.Value == p.SelImage {
			initialImage = opt
		}
	}
	if initialImage == nil && len(imageOpts) > 0 {
		initialImage = imageOpts[0]
	}
	blocks = append(blocks,
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", "*Base image*", false, false),
			nil, nil,
		),
	)
	imageRadio := slack.NewRadioButtonsBlockElement("init_image", imageOpts...)
	imageRadio.InitialOption = initialImage
	blocks = append(blocks, slack.NewActionBlock("init_image_row", imageRadio))

	// SSH (radio).
	sshOpts := make([]*slack.OptionBlockObject, 0, len(initSSHOptions))
	var initialSSH *slack.OptionBlockObject
	for _, o := range initSSHOptions {
		label := truncateForSlackText(o.Label, 75)
		desc := truncateForSlackText(o.Description, 75)
		opt := slack.NewOptionBlockObject(
			o.Value,
			slack.NewTextBlockObject("plain_text", label, false, false),
			slack.NewTextBlockObject("plain_text", desc, false, false),
		)
		sshOpts = append(sshOpts, opt)
		if o.Value == p.SelSSH {
			initialSSH = opt
		}
	}
	if initialSSH == nil && len(sshOpts) > 0 {
		initialSSH = sshOpts[0]
	}
	blocks = append(blocks,
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", "*SSH credential forwarding*", false, false),
			nil, nil,
		),
	)
	sshRadio := slack.NewRadioButtonsBlockElement("init_ssh", sshOpts...)
	sshRadio.InitialOption = initialSSH
	blocks = append(blocks, slack.NewActionBlock("init_ssh_row", sshRadio))

	// Model (radio). The selection becomes the thread's stored Model
	// preference and gets forwarded to claude as --model on every run
	// until the user switches via reaction emoji.
	modelOpts := make([]*slack.OptionBlockObject, 0, len(initModelOptions))
	var initialModel *slack.OptionBlockObject
	for _, o := range initModelOptions {
		label := truncateForSlackText(o.Label, 75)
		desc := truncateForSlackText(o.Description, 75)
		opt := slack.NewOptionBlockObject(
			o.Value,
			slack.NewTextBlockObject("plain_text", label, false, false),
			slack.NewTextBlockObject("plain_text", desc, false, false),
		)
		modelOpts = append(modelOpts, opt)
		if o.Value == p.SelModel {
			initialModel = opt
		}
	}
	if initialModel == nil && len(modelOpts) > 0 {
		initialModel = modelOpts[0]
	}
	blocks = append(blocks,
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", "*Model*", false, false),
			nil, nil,
		),
	)
	modelRadio := slack.NewRadioButtonsBlockElement("init_model", modelOpts...)
	modelRadio.InitialOption = initialModel
	blocks = append(blocks, slack.NewActionBlock("init_model_row", modelRadio))

	// Template (radio). If there are sibling tasks, offer them as a
	// starting point — picking one copies that directory's contents
	// (excluding per-instance `.clod/` state) before the .clod config
	// files below are written from the other pickers.
	if len(p.Templates) > 0 {
		tplOpts := make([]*slack.OptionBlockObject, 0, len(p.Templates)+1)
		noneOpt := slack.NewOptionBlockObject(
			"",
			slack.NewTextBlockObject("plain_text", "(none)", false, false),
			slack.NewTextBlockObject("plain_text", "Start from an empty directory.", false, false),
		)
		tplOpts = append(tplOpts, noneOpt)
		var initialTpl *slack.OptionBlockObject = noneOpt
		for _, t := range p.Templates {
			label := truncateForSlackText(t, 75)
			desc := truncateForSlackText(fmt.Sprintf("Copy contents of `%s` as a starting point.", t), 75)
			opt := slack.NewOptionBlockObject(
				t,
				slack.NewTextBlockObject("plain_text", label, false, false),
				slack.NewTextBlockObject("plain_text", desc, false, false),
			)
			tplOpts = append(tplOpts, opt)
			if t == p.SelTemplate {
				initialTpl = opt
			}
		}
		blocks = append(blocks,
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", "*Template* (optional)\n_Copies the chosen task's files into the new directory. Per-task `.clod/` state is regenerated._", false, false),
				nil, nil,
			),
		)
		tplRadio := slack.NewRadioButtonsBlockElement("init_template", tplOpts...)
		tplRadio.InitialOption = initialTpl
		blocks = append(blocks, slack.NewActionBlock("init_template_row", tplRadio))
	}

	// Packages (checkboxes). Values are indices into p.Packages so the
	// label length doesn't blow past the 75-char option-text cap.
	if len(p.Packages) > 0 {
		pkgOpts := make([]*slack.OptionBlockObject, 0, len(p.Packages))
		var initialPkgs []*slack.OptionBlockObject
		preSel := map[string]bool{}
		for _, s := range p.SelPackages {
			preSel[s] = true
		}
		for i, pkg := range p.Packages {
			val := fmt.Sprintf("%d", i)
			opt := slack.NewOptionBlockObject(
				val,
				slack.NewTextBlockObject("plain_text", truncateForSlackText(pkg, 75), false, false),
				nil,
			)
			pkgOpts = append(pkgOpts, opt)
			if preSel[val] {
				initialPkgs = append(initialPkgs, opt)
			}
		}
		blocks = append(blocks,
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", "*Extra packages* (installed via `apt-get install`)", false, false),
				nil, nil,
			),
		)
		pkgBox := slack.NewCheckboxGroupsBlockElement("init_packages", pkgOpts...)
		if len(initialPkgs) > 0 {
			pkgBox.InitialOptions = initialPkgs
		}
		blocks = append(blocks, slack.NewActionBlock("init_packages_row", pkgBox))
	}

	// Submit / Cancel.
	createValue := fmt.Sprintf(`{"k":%q,"b":"allow"}`, progressKey)
	cancelValue := fmt.Sprintf(`{"k":%q,"b":"deny"}`, progressKey)
	createBtn := slack.NewButtonBlockElement(
		"init_create",
		createValue,
		slack.NewTextBlockObject("plain_text", "Create and run task", false, false),
	)
	createBtn.Style = "primary"
	cancelBtn := slack.NewButtonBlockElement(
		"init_cancel",
		cancelValue,
		slack.NewTextBlockObject("plain_text", "Cancel", false, false),
	)
	cancelBtn.Style = "danger"
	blocks = append(blocks, slack.NewActionBlock("init_submit_row", createBtn, cancelBtn))

	return blocks
}

// requiredAptPackages used to force python3 into every Dockerfile_project
// because the MCP permission bridge was a Python script. The bridge has
// since been rewritten as a static linux/amd64 Go binary that runs
// without any interpreter, so nothing is forcibly injected anymore — the
// user's package list is what goes into the image.
var requiredAptPackages = []string{}

// writeInitFiles materializes the `.clod/` directory (and the task dir if
// CreateDir is set) using the user's selections. Mirrors the file layout
// that `bin/clod`'s `initialize()` produces; clod's next invocation will
// detect the missing `.clod/system/` and populate it automatically.
func writeInitFiles(p *pendingInit, image, sshMode string, packages []string) error {
	if p.CreateDir {
		if err := os.MkdirAll(p.TaskPath, 0o755); err != nil {
			return fmt.Errorf("create task dir: %w", err)
		}
	}

	clodDir := filepath.Join(p.TaskPath, ".clod")
	if err := os.MkdirAll(filepath.Join(clodDir, "claude"), 0o755); err != nil {
		return fmt.Errorf("create .clod/claude: %w", err)
	}

	writeFile := func(rel, content string) error {
		full := filepath.Join(clodDir, rel)
		return os.WriteFile(full, []byte(content), 0o644)
	}

	if err := writeFile("name", p.TaskName+"\n"); err != nil {
		return err
	}
	if err := writeFile("image", image+"\n"); err != nil {
		return err
	}
	if err := writeFile("ssh", sshMode+"\n"); err != nil {
		return err
	}

	// Merge required + user-selected packages, dedup, sort.
	seen := map[string]bool{}
	var merged []string
	for _, pkg := range requiredAptPackages {
		if !seen[pkg] {
			seen[pkg] = true
			merged = append(merged, pkg)
		}
	}
	for _, pkg := range packages {
		if !seen[pkg] {
			seen[pkg] = true
			merged = append(merged, pkg)
		}
	}
	sort.Strings(merged)

	var dockerfileProject string
	if len(merged) > 0 {
		var pkgLines strings.Builder
		for _, pkg := range merged {
			pkgLines.WriteString("      " + pkg + " \\\n")
		}
		pkgs := strings.TrimRight(pkgLines.String(), " \\\n")
		dockerfileProject = "FROM base AS project\n\n" +
			"ARG DEBIAN_FRONTEND=noninteractive\n" +
			"RUN --mount=type=cache,sharing=locked,target=/var/cache/apt \\\n" +
			"    --mount=type=cache,sharing=locked,target=/var/lib/apt \\\n" +
			"    apt-get update \\\n" +
			" && apt-get install -qq -y \\\n" +
			pkgs + "\n"
	} else {
		// No packages selected — leave a commented-out template the user
		// can edit later. The bot no longer needs any packages in the
		// image for its own operation (the MCP bridge is a static Go
		// binary injected at runtime).
		dockerfileProject = "FROM base AS project\n\n" +
			"# Uncomment to add project-specific dependencies.\n" +
			"#ARG DEBIAN_FRONTEND=noninteractive\n" +
			"#RUN --mount=type=cache,sharing=locked,target=/var/cache/apt \\\n" +
			"#    --mount=type=cache,sharing=locked,target=/var/lib/apt \\\n" +
			"#    apt-get update \\\n" +
			"# && apt-get install -qq -y jq\n"
	}
	if err := writeFile("Dockerfile_project", dockerfileProject); err != nil {
		return err
	}

	// Dockerfile_user: minimal template matching clod's default.
	dockerfileUser := "FROM wrapper AS user\n\n" +
		"# Add user-specific customizations here.\n" +
		"# This layer runs as the non-root user (after USER switch).\n"
	if err := writeFile("Dockerfile_user", dockerfileUser); err != nil {
		return err
	}

	// README.md stub at the task dir root. The bot's agent-prompt default
	// is README.md; having at least a stub silences the "agent prompt file
	// not found" warning and gives the user a place to document the task.
	readmePath := filepath.Join(p.TaskPath, "README.md")
	if _, err := os.Stat(readmePath); os.IsNotExist(err) {
		stub := fmt.Sprintf("# %s\n\nAdd task-specific instructions and context for the agent here.\n", p.TaskName)
		if err := os.WriteFile(readmePath, []byte(stub), 0o644); err != nil {
			return fmt.Errorf("write README.md: %w", err)
		}
	}

	return nil
}

// copyTaskTemplate clones the contents of src into dst, skipping paths
// under `.clod/` that carry per-instance state (id, system/, runtime-*,
// claude/). Called before writeInitFiles so the subsequent `.clod/` files
// written from the user's picks land on top of the template — if the
// template included those same files they get overwritten with the
// current selections, which is what we want.
func copyTaskTemplate(src, dst string) error {
	srcAbs, err := filepath.Abs(src)
	if err != nil {
		return fmt.Errorf("abs template src: %w", err)
	}
	info, err := os.Stat(srcAbs)
	if err != nil {
		return fmt.Errorf("stat template src %s: %w", srcAbs, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("template src is not a directory: %s", srcAbs)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("create template dst: %w", err)
	}

	return filepath.WalkDir(srcAbs, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcAbs, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if isPerInstanceClodPath(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			// 0o755 is fine for new task dirs — matches what clod's
			// init creates. Preserving exact template perms isn't
			// worth the complexity.
			return os.MkdirAll(target, 0o755)
		}
		return copyRegularFile(path, target)
	})
}

// isPerInstanceClodPath returns true for `.clod/` subpaths that must NOT
// carry across when cloning a task. These are regenerated by clod on the
// new task's first run; copying them would share docker image names,
// credentials, or cached build state with the template.
func isPerInstanceClodPath(rel string) bool {
	skips := []string{
		filepath.Join(".clod", "id"),
		filepath.Join(".clod", "hash"),
		filepath.Join(".clod", "system"),
		filepath.Join(".clod", "claude"),
	}
	for _, s := range skips {
		if rel == s || strings.HasPrefix(rel, s+string(filepath.Separator)) {
			return true
		}
	}
	// runtime-XXX directories are per-invocation scratch space.
	if strings.HasPrefix(rel, filepath.Join(".clod", "runtime-")) {
		return true
	}
	return false
}

func copyRegularFile(srcPath, dstPath string) error {
	info, err := os.Stat(srcPath)
	if err != nil {
		return err
	}
	// Symlinks: preserve as symlinks (don't follow + re-copy a potentially
	// giant target).
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(srcPath)
		if err != nil {
			return err
		}
		_ = os.Remove(dstPath)
		return os.Symlink(target, dstPath)
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return err
	}
	s, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer s.Close()
	d, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer d.Close()
	_, err = io.Copy(d, s)
	return err
}
