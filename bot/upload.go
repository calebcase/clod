package main

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/calebcase/oops"
	"github.com/rs/zerolog"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// uploadZipThreshold is the number of files above which a directory
// upload zips first instead of uploading individual files. Picked
// to keep small uploads scrollable and large uploads from posting
// hundreds of separate file-share messages.
const uploadZipThreshold = 5

// uploadZipDir is the staging directory for archive files we
// generate. /tmp is intentional: the file-sync watcher only
// monitors task directories, so a zip living in /tmp can't get
// re-uploaded as part of normal sync. Cleaned up after the upload
// either succeeds or fails.
const uploadZipDir = "/tmp"

// pendingUpload tracks an outstanding `@bot upload <dir>` dialog
// while the user picks recursive vs top-level. Path is the
// directory being uploaded; channel/thread/message-ts let the
// click handler update the prompt and post results in the right
// thread.
type pendingUpload struct {
	Path        string
	ChannelID   string
	ThreadTS    string
	MessageTS   string
	RequesterID string
}

// handleUploadCommand routes the `@bot upload <path>` mention. For
// a regular file: upload immediately, no dialog. For a directory:
// post a recursive-vs-top-level confirmation dialog and stash
// pending state for the click handler. For a missing or
// unreadable path: post a friendly error and bail.
func (h *Handler) handleUploadCommand(
	ctx context.Context,
	ev *slackevents.AppMentionEvent,
	threadTS string,
	path string,
	logger zerolog.Logger,
) {
	if path == "" {
		if _, err := h.bot.PostMessage(ev.Channel,
			":warning: `upload` needs a path. Try `@bot upload /some/file-or-dir`.",
			threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post upload-empty-path warning")
		}
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		if _, perr := h.bot.PostMessage(ev.Channel,
			fmt.Sprintf(":warning: Can't read `%s`: %v", path, err),
			threadTS); perr != nil {
			logger.Debug().Err(perr).Msg("failed to post upload-stat-error")
		}
		return
	}
	if !info.IsDir() {
		// File: upload directly without dialog.
		h.executeUpload(ev.Channel, threadTS, path, false, false, logger)
		return
	}

	// Directory: ask the user how to enumerate it.
	progressKey := key(ev.Channel, threadTS)
	if _, already := h.pendingUploads.Load(progressKey); already {
		if _, err := h.bot.PostMessage(ev.Channel,
			":warning: An upload confirmation is already pending on this thread.",
			threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post duplicate-upload warning")
		}
		return
	}

	allowValue := fmt.Sprintf(`{"k":%q,"b":"allow"}`, progressKey)
	denyValue := fmt.Sprintf(`{"k":%q,"b":"deny"}`, progressKey)

	header := slack.NewSectionBlock(
		slack.NewTextBlockObject(
			"mrkdwn",
			fmt.Sprintf(":outbox_tray: *Upload* `%s`\n_How do you want to enumerate this directory? Files in the result will be uploaded individually unless there are more than %d, in which case I'll zip and upload that instead._",
				path, uploadZipThreshold),
			false, false,
		),
		nil, nil,
	)
	recBtn := slack.NewButtonBlockElement(
		"upload_recursive",
		allowValue,
		slack.NewTextBlockObject("plain_text", "Recursive", false, false),
	)
	recBtn.Style = "primary"
	topBtn := slack.NewButtonBlockElement(
		"upload_toplevel",
		allowValue,
		slack.NewTextBlockObject("plain_text", "Top-level only", false, false),
	)
	cancelBtn := slack.NewButtonBlockElement(
		"upload_cancel",
		denyValue,
		slack.NewTextBlockObject("plain_text", "Cancel", false, false),
	)
	cancelBtn.Style = "danger"
	actions := slack.NewActionBlock("upload_actions", recBtn, topBtn, cancelBtn)

	msgTS, err := h.bot.PostMessageBlocks(ev.Channel, []slack.Block{header, actions}, threadTS)
	if err != nil {
		logger.Error().Err(err).Msg("failed to post upload confirmation dialog")
		return
	}
	h.pendingUploads.Store(progressKey, &pendingUpload{
		Path:        path,
		ChannelID:   ev.Channel,
		ThreadTS:    threadTS,
		MessageTS:   msgTS,
		RequesterID: ev.User,
	})
}

// handleUploadFinal resolves an upload dialog click. Recursive and
// top-level both proceed to executeUpload with the corresponding
// flag; cancel posts an outcome and drops state. Only the original
// requester can pick recursive/top-level; anyone in the thread can
// cancel.
func (h *Handler) handleUploadFinal(
	callback *slack.InteractionCallback,
	action *slack.BlockAction,
	actionValue PermissionActionValue,
	logger zerolog.Logger,
) {
	stateVal, ok := h.pendingUploads.LoadAndDelete(actionValue.ThreadKey)
	if !ok {
		logger.Warn().Msg("no pending upload found; button is stale")
		if err := h.bot.UpdateMessage(callback.Channel.ID, callback.Message.Timestamp,
			":warning: This upload confirmation is no longer active."); err != nil {
			logger.Error().Err(err).Msg("failed to update stale upload message")
		}
		return
	}
	state := stateVal.(*pendingUpload)

	if action.ActionID != "upload_cancel" && callback.User.ID != state.RequesterID {
		// Non-requester clicked Recursive/Top-level: refuse and put
		// state back so the requester can still confirm.
		logger.Warn().
			Str("clicked_by", callback.User.ID).
			Str("requester", state.RequesterID).
			Msg("non-requester tried to resolve upload dialog")
		h.pendingUploads.Store(actionValue.ThreadKey, state)
		return
	}

	switch action.ActionID {
	case "upload_cancel":
		outcome := fmt.Sprintf(":x: Upload cancelled by <@%s>.", callback.User.ID)
		if err := h.bot.UpdateMessage(state.ChannelID, state.MessageTS, outcome); err != nil {
			logger.Debug().Err(err).Msg("failed to update cancelled upload message")
		}
		return
	case "upload_recursive":
		outcome := fmt.Sprintf(":outbox_tray: Uploading `%s` (recursive) by <@%s>…", state.Path, callback.User.ID)
		if err := h.bot.UpdateMessage(state.ChannelID, state.MessageTS, outcome); err != nil {
			logger.Debug().Err(err).Msg("failed to update upload-recursive message")
		}
		h.executeUpload(state.ChannelID, state.ThreadTS, state.Path, true, true, logger)
	case "upload_toplevel":
		outcome := fmt.Sprintf(":outbox_tray: Uploading `%s` (top-level only) by <@%s>…", state.Path, callback.User.ID)
		if err := h.bot.UpdateMessage(state.ChannelID, state.MessageTS, outcome); err != nil {
			logger.Debug().Err(err).Msg("failed to update upload-toplevel message")
		}
		h.executeUpload(state.ChannelID, state.ThreadTS, state.Path, true, false, logger)
	}
}

// executeUpload is the workhorse: enumerate the target, decide
// individual-vs-zip based on file count, perform the uploads, and
// post status / errors back to the thread. isDir controls how
// enumeration treats `path`; recursive is honored only when
// isDir is true.
func (h *Handler) executeUpload(
	channelID, threadTS, path string,
	isDir, recursive bool,
	logger zerolog.Logger,
) {
	if !isDir {
		h.uploadOneFile(channelID, threadTS, path, fmt.Sprintf(":outbox_tray: Upload: `%s`", filepath.Base(path)), logger)
		return
	}
	files, err := enumerateUploadFiles(path, recursive)
	if err != nil {
		logger.Error().Err(err).Str("path", path).Msg("failed to enumerate upload directory")
		if _, perr := h.bot.PostMessage(channelID,
			fmt.Sprintf(":warning: Couldn't enumerate `%s`: %v", path, err), threadTS); perr != nil {
			logger.Debug().Err(perr).Msg("failed to post enumerate-error")
		}
		return
	}
	if len(files) == 0 {
		if _, err := h.bot.PostMessage(channelID,
			fmt.Sprintf(":information_source: `%s` contains no files to upload.", path), threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post empty-dir notice")
		}
		return
	}

	if len(files) > uploadZipThreshold {
		// Stream progress lines through the same rolling-tail
		// widget the docker build uses, so the user sees per-file
		// progress as the archive is built. Header reflects the
		// source dir so concurrent uploads (unlikely) stay
		// distinguishable. Finalize at the end with a summary.
		header := fmt.Sprintf(":hourglass_flowing_sand: *Zipping `%s`* (%d files)", path, len(files))
		emit := func(line string) {
			h.updateNamedProgressMessage(channelID, threadTS, "zip", header, line, logger)
		}
		zipPath, totalBytes, err := zipDirForUpload(path, files, emit)
		if err != nil {
			h.finalizeNamedProgressMessage(channelID, threadTS, "zip",
				fmt.Sprintf(":x: *Zip failed* `%s`: %v", path, err), logger)
			logger.Error().Err(err).Str("path", path).Msg("failed to zip upload directory")
			if _, perr := h.bot.PostMessage(channelID,
				fmt.Sprintf(":warning: Couldn't zip `%s`: %v", path, err), threadTS); perr != nil {
				logger.Debug().Err(perr).Msg("failed to post zip-error")
			}
			return
		}
		zipInfo, _ := os.Stat(zipPath)
		var compressedBytes int64
		if zipInfo != nil {
			compressedBytes = zipInfo.Size()
		}
		h.finalizeNamedProgressMessage(channelID, threadTS, "zip",
			fmt.Sprintf(":package: *Zipped* `%s` — %d files, %s in → %s out",
				filepath.Base(path), len(files),
				humanBytes(totalBytes), humanBytes(compressedBytes)), logger)
		// Always remove the staging zip — keeping it around in /tmp
		// would leak disk space and (more importantly) potentially
		// be picked up by a future filesync watcher if /tmp ever
		// gets bind-mounted or aliased.
		defer func() {
			if err := os.Remove(zipPath); err != nil {
				logger.Debug().Err(err).Str("path", zipPath).Msg("failed to remove staging zip")
			}
		}()
		comment := fmt.Sprintf(":outbox_tray: Upload: `%s` (%d files zipped)", filepath.Base(path), len(files))
		h.uploadOneFile(channelID, threadTS, zipPath, comment, logger)
		return
	}

	// Few-enough files: upload individually.
	for _, f := range files {
		rel, err := filepath.Rel(path, f)
		if err != nil {
			rel = filepath.Base(f)
		}
		h.uploadOneFile(channelID, threadTS, f, fmt.Sprintf(":outbox_tray: Upload: `%s`", rel), logger)
	}
}

// uploadOneFile is a thin wrapper around FileHandler.UploadFromTaskOutputs
// that translates errors into thread-visible warnings instead of
// silently logging.
func (h *Handler) uploadOneFile(channelID, threadTS, path, comment string, logger zerolog.Logger) {
	if _, err := h.bot.files.UploadFromTaskOutputs(path, channelID, threadTS, comment); err != nil {
		logger.Error().Err(err).Str("path", path).Msg("failed to upload requested file")
		if _, perr := h.bot.PostMessage(channelID,
			fmt.Sprintf(":warning: Failed to upload `%s`: %v", path, err), threadTS); perr != nil {
			logger.Debug().Err(perr).Msg("failed to post upload-error")
		}
	}
}

// enumerateUploadFiles returns absolute paths of every regular file
// under root (one level deep when recursive is false, full tree
// when true). Symlinks are ignored to avoid cycles. Hidden files
// (dotfiles) are kept — the caller asked to upload the directory,
// hiding things they're aware of would surprise them.
func enumerateUploadFiles(root string, recursive bool) ([]string, error) {
	var files []string
	if !recursive {
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, oops.Trace(err)
		}
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			files = append(files, filepath.Join(root, e.Name()))
		}
		return files, nil
	}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.Type().IsRegular() {
			return nil
		}
		files = append(files, p)
		return nil
	})
	if err != nil {
		return nil, oops.Trace(err)
	}
	return files, nil
}

// zipDirForUpload writes a zip archive of `files` (which should all
// be under `root`) to a unique path under uploadZipDir. Entries
// inside the archive are stored relative to root so the receiver
// gets the original layout when unzipped. The emit callback is
// invoked once per added file with a short progress line — used by
// the upload command's rolling Slack progress widget. Returns the
// absolute zip path and the total uncompressed bytes written;
// caller is responsible for removing the file after upload.
func zipDirForUpload(root string, files []string, emit func(line string)) (string, int64, error) {
	stamp := time.Now().UTC().Format("20060102-150405")
	base := strings.ReplaceAll(filepath.Base(root), "/", "_")
	if base == "" || base == "." {
		base = "upload"
	}
	zipName := fmt.Sprintf("clod-upload-%s-%s", stamp, base)
	zipPath := filepath.Join(uploadZipDir, zipName+".zip")

	// rootPrefix is the single top-level directory every archive
	// entry sits under, so unzipping always materializes one
	// directory rather than spraying files into the current
	// working dir (the "tarbomb" pattern). Matches the zip
	// filename (sans extension) so the unpacked dir is
	// self-identifying — `unzip foo.zip && ls foo/` Just Works.
	rootPrefix := zipName

	out, err := os.Create(zipPath)
	if err != nil {
		return "", 0, oops.Trace(err)
	}
	zw := zip.NewWriter(out)

	if emit != nil {
		emit(fmt.Sprintf("[zip] target: %s", zipPath))
		emit(fmt.Sprintf("[zip] entries will unpack under %s/", rootPrefix))
	}

	var totalBytes int64
	closeAll := func(err error) (string, int64, error) {
		_ = zw.Close()
		_ = out.Close()
		if err != nil {
			_ = os.Remove(zipPath)
		}
		return zipPath, totalBytes, err
	}

	// Add the top-level directory as an explicit entry so
	// extraction tools that don't infer parents from file paths
	// still create it.
	if _, err := zw.Create(rootPrefix + "/"); err != nil {
		return closeAll(oops.Trace(err))
	}

	for i, f := range files {
		rel, err := filepath.Rel(root, f)
		if err != nil {
			return closeAll(oops.Trace(err))
		}
		// Use forward slashes inside the archive for portability,
		// regardless of host OS, and prefix every entry with the
		// containing directory so extraction is self-contained.
		rel = filepath.ToSlash(rel)
		entryName := rootPrefix + "/" + rel
		fi, err := os.Stat(f)
		if err != nil {
			return closeAll(oops.Trace(err))
		}
		hdr, err := zip.FileInfoHeader(fi)
		if err != nil {
			return closeAll(oops.Trace(err))
		}
		hdr.Name = entryName
		hdr.Method = zip.Deflate
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return closeAll(oops.Trace(err))
		}
		src, err := os.Open(f)
		if err != nil {
			return closeAll(oops.Trace(err))
		}
		n, copyErr := io.Copy(w, src)
		_ = src.Close()
		if copyErr != nil {
			return closeAll(oops.Trace(copyErr))
		}
		totalBytes += n
		if emit != nil {
			emit(fmt.Sprintf("[zip] (%d/%d) %s · %s", i+1, len(files), entryName, humanBytes(n)))
		}
	}
	if err := zw.Close(); err != nil {
		_ = out.Close()
		_ = os.Remove(zipPath)
		return "", totalBytes, oops.Trace(err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(zipPath)
		return "", totalBytes, oops.Trace(err)
	}
	return zipPath, totalBytes, nil
}

// humanBytes renders a byte count as a short human-readable string
// (e.g. "1.2 MB", "873 B"). Used by the zip progress lines and the
// "zipped" finalize message.
func humanBytes(n int64) string {
	const (
		_  = iota
		kB = 1 << (10 * iota)
		mB
		gB
	)
	switch {
	case n >= gB:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(gB))
	case n >= mB:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(mB))
	case n >= kB:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(kB))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
