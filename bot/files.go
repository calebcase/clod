package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/calebcase/oops"
	"github.com/rs/zerolog"
	"github.com/slack-go/slack"
)

// FileHandler manages file transfers between Slack and task directories.
// It optionally holds a back-reference to Bot so the output-file
// watcher can use Bot.LatestPostTS / Bot.PostMessage / Bot.UpdateMessage
// when editing-in-place repeated uploads of the same file. bot is
// populated post-construction (circular reference) via AttachBot
// because Bot itself instantiates FileHandler.
type FileHandler struct {
	client *slack.Client
	logger zerolog.Logger
	bot    *Bot
}

// NewFileHandler creates a new FileHandler.
func NewFileHandler(client *slack.Client, logger zerolog.Logger) *FileHandler {
	return &FileHandler{
		client: client,
		logger: logger.With().Str("component", "files").Logger(),
	}
}

// AttachBot wires the back-reference so the output watcher can
// coordinate with the rest of the bot's post-tracking state. Called
// from NewBot after both objects exist.
func (f *FileHandler) AttachBot(bot *Bot) {
	f.bot = bot
}

// DownloadedFile represents a file downloaded from Slack.
type DownloadedFile struct {
	Name      string
	MimeType  string
	Data      []byte
	LocalPath string // Only set if saved to disk
}

// uploadedFile tracks upload state for output file watching.
type uploadedFile struct {
	modTime        time.Time // Last modification time when uploaded
	lastUploadTime time.Time // When the file was last uploaded (for rate limiting)
}

// bundleMode distinguishes the two consolidation strategies. Consecutive
// syncs stay in the same bundle only when their mode matches, so an
// inline-text sync followed by a binary-image sync starts a new bundle.
type bundleMode string

const (
	bundleModeInline    bundleMode = "inline"
	bundleModeFileshare bundleMode = "fileshare"
)

// inlineEntry is one file's contribution to an inline-mode bundle.
// content is post-trim (no trailing newlines) so the code fence stays
// flush.
type inlineEntry struct {
	name    string
	content []byte
}

// fileshareEntry is one file's contribution to a fileshare-mode bundle.
// fileID is the Slack file ID returned by files.getUploadURLExternal;
// title is what appears above the preview.
type fileshareEntry struct {
	name   string
	fileID string
	title  string
}

// outputBundle tracks a consolidated output message that groups
// consecutive file syncs into a single Slack message. When the bot's
// latest post is still this bundle's message (i.e. no other message
// type interleaved) and the incoming sync's mode matches, the next
// file extends the bundle:
//
//   - inline mode: edit the message in place, appending or
//     replacing the file's code block.
//   - fileshare mode: delete the prior message and re-post via
//     files.completeUploadExternal with the accumulated file IDs so
//     Slack creates one message showing every file at once.
//
// A mismatched mode, an interleaving message, or a size-overflow
// starts a fresh bundle.
type outputBundle struct {
	messageTS string
	mode      bundleMode
	inline    []inlineEntry    // populated when mode == bundleModeInline
	fileshare []fileshareEntry // populated when mode == bundleModeFileshare
}

// DownloadToMemory downloads a Slack file to memory using the slack-go client.
// Returns the file data and metadata without writing to disk.
func (f *FileHandler) DownloadToMemory(file slack.File) (*DownloadedFile, error) {
	f.logger.Info().
		Str("file_id", file.ID).
		Str("filename", file.Name).
		Int("size", file.Size).
		Str("mimetype", file.Mimetype).
		Msg("downloading file from Slack to memory")

	// Use URLPrivateDownload which is the download-specific URL.
	url := file.URLPrivateDownload
	if url == "" {
		url = file.URLPrivate
	}
	if url == "" {
		return nil, oops.New("no download URL available for file %s", file.ID)
	}

	f.logger.Debug().
		Str("url", url).
		Msg("fetching file via client.GetFile")

	// Use slack-go's GetFile method which handles authentication properly.
	var buf bytes.Buffer
	if err := f.client.GetFile(url, &buf); err != nil {
		return nil, oops.Trace(err)
	}

	data := buf.Bytes()

	f.logger.Info().
		Int("bytes_read", len(data)).
		Str("mimetype", file.Mimetype).
		Msg("file downloaded to memory successfully")

	return &DownloadedFile{
		Name:     file.Name,
		MimeType: file.Mimetype,
		Data:     data,
	}, nil
}

// DownloadToTask downloads a Slack file to the task directory.
// Returns the local file path where the file was saved.
// If a file with the same name already exists, an auto-incrementing number is added
// (e.g., image.png, image-1.png, image-2.png).
func (f *FileHandler) DownloadToTask(file slack.File, taskPath string) (localPath string, err error) {
	// Determine the filename (use Slack's filename, sanitize if needed).
	filename := file.Name
	if filename == "" {
		filename = file.ID
	}
	localPath = filepath.Join(taskPath, filename)

	// If file already exists, add auto-incrementing number before extension.
	if _, err := os.Stat(localPath); err == nil {
		ext := filepath.Ext(filename)
		base := filename[:len(filename)-len(ext)]
		for i := 1; ; i++ {
			newFilename := fmt.Sprintf("%s-%d%s", base, i, ext)
			localPath = filepath.Join(taskPath, newFilename)
			if _, err := os.Stat(localPath); os.IsNotExist(err) {
				filename = newFilename
				break
			}
		}
	}

	f.logger.Info().
		Str("file_id", file.ID).
		Str("filename", filename).
		Str("local_path", localPath).
		Int("size", file.Size).
		Str("mimetype", file.Mimetype).
		Msg("downloading file from Slack to disk")

	// Use URLPrivateDownload which is the download-specific URL.
	url := file.URLPrivateDownload
	if url == "" {
		url = file.URLPrivate
	}
	if url == "" {
		return "", oops.New("no download URL available for file %s", file.ID)
	}

	f.logger.Debug().
		Str("url", url).
		Msg("fetching file via client.GetFile")

	// Create the local file.
	out, err := os.Create(localPath)
	if err != nil {
		return "", oops.Trace(err)
	}
	defer func() {
		oops.ChainP(&err, out.Close())
	}()

	// Use slack-go's GetFile method which handles authentication properly.
	if err = f.client.GetFile(url, out); err != nil {
		return "", oops.Trace(err)
	}

	// Get file size for logging.
	var info os.FileInfo
	info, err = out.Stat()
	if err != nil {
		return "", oops.Trace(err)
	}

	f.logger.Info().
		Str("local_path", localPath).
		Int64("bytes_written", info.Size()).
		Msg("file downloaded successfully")

	return
}

// UploadFromTaskOutputs uploads a file from the task's outputs directory to Slack.
func (f *FileHandler) UploadFromTaskOutputs(
	localPath string,
	channelID string,
	threadTS string,
	comment string,
) (*slack.FileSummary, error) {
	f.logger.Info().
		Str("local_path", localPath).
		Str("channel", channelID).
		Str("thread_ts", threadTS).
		Msg("uploading file to Slack")

	// Get file info.
	info, err := os.Stat(localPath)
	if err != nil {
		return nil, oops.Trace(err)
	}

	// Use UploadFileV2 (the new API).
	params := slack.UploadFileV2Parameters{
		File:            localPath,
		FileSize:        int(info.Size()),
		Filename:        filepath.Base(localPath),
		Title:           filepath.Base(localPath),
		Channel:         channelID,
		ThreadTimestamp: threadTS,
		InitialComment:  comment,
	}

	summary, err := f.client.UploadFileV2(params)
	if err != nil {
		return nil, oops.Trace(err)
	}

	f.logger.Info().
		Str("file_id", summary.ID).
		Str("title", summary.Title).
		Msg("file uploaded successfully")

	return summary, nil
}

// UploadSnippet uploads text content as a collapsible snippet to Slack.
// This is useful for tool output that would be too long for inline display.
// The comment parameter is shown as a message alongside the file.
func (f *FileHandler) UploadSnippet(
	content string,
	title string,
	comment string,
	channelID string,
	threadTS string,
) (*slack.FileSummary, error) {
	f.logger.Debug().
		Int("content_len", len(content)).
		Str("title", title).
		Str("channel", channelID).
		Msg("uploading snippet to Slack")

	params := slack.UploadFileV2Parameters{
		Content:         content,
		FileSize:        len(content),
		Filename:        title + ".txt",
		Title:           title,
		InitialComment:  comment,
		Channel:         channelID,
		ThreadTimestamp: threadTS,
	}

	summary, err := f.client.UploadFileV2(params)
	if err != nil {
		return nil, oops.Trace(err)
	}

	f.logger.Debug().
		Str("file_id", summary.ID).
		Msg("snippet uploaded successfully")

	return summary, nil
}

// GetMessageFiles fetches the full message to get file information.
// This is needed because app_mention events don't include the files array.
func (f *FileHandler) GetMessageFiles(channelID, messageTS string) ([]slack.File, error) {
	// Use conversations.history with a very small window around the message.
	params := &slack.GetConversationHistoryParameters{
		ChannelID: channelID,
		Latest:    messageTS,
		Oldest:    messageTS,
		Inclusive: true,
		Limit:     1,
	}

	history, err := f.client.GetConversationHistory(params)
	if err != nil {
		return nil, oops.Trace(err)
	}

	if len(history.Messages) == 0 {
		return nil, nil
	}

	msg := history.Messages[0]
	if len(msg.Files) > 0 {
		f.logger.Debug().
			Int("num_files", len(msg.Files)).
			Str("message_ts", messageTS).
			Msg("found files in message")
	}

	return msg.Files, nil
}

// GetThreadReplyFiles fetches files from a thread reply.
func (f *FileHandler) GetThreadReplyFiles(channelID, threadTS, messageTS string) ([]slack.File, error) {
	// Use conversations.replies to get the specific message in the thread.
	params := &slack.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: threadTS,
		Latest:    messageTS,
		Oldest:    messageTS,
		Inclusive: true,
		Limit:     1,
	}

	msgs, _, _, err := f.client.GetConversationReplies(params)
	if err != nil {
		return nil, oops.Trace(err)
	}

	// Find the specific message by timestamp.
	for _, msg := range msgs {
		if msg.Timestamp == messageTS {
			if len(msg.Files) > 0 {
				f.logger.Debug().
					Int("num_files", len(msg.Files)).
					Str("message_ts", messageTS).
					Msg("found files in thread reply")
			}
			return msg.Files, nil
		}
	}

	return nil, nil
}

// WatchOutputs monitors the task directory for new files and uploads them.
// This is intended to run in a goroutine during task execution.
//
// shouldSync is consulted on every poll tick. When it returns false the
// watcher still tracks existing file state (so re-enabling doesn't cause
// a flood of retroactive uploads) but skips the actual upload step.
// When shouldSync is nil the watcher always uploads.
func (f *FileHandler) WatchOutputs(
	taskPath string,
	channelID string,
	threadTS string,
	done <-chan struct{},
	shouldSync func() bool,
) {
	// Track files we've already uploaded with their modification times.
	uploaded := make(map[string]*uploadedFile)

	// Consolidation state — grows across ticks. bundle == nil means
	// "no current output message"; the next sync starts a fresh one.
	var bundle *outputBundle

	// Get initial file list to avoid uploading pre-existing files.
	entries, _ := os.ReadDir(taskPath)
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			uploaded[e.Name()] = &uploadedFile{
				modTime:        info.ModTime(),
				lastUploadTime: time.Now(),
			}
		}
	}

	f.logger.Debug().
		Str("task_path", taskPath).
		Int("existing_files", len(uploaded)).
		Msg("starting output file watcher")

	// Poll for new files until done.
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	tick := func() {
		if shouldSync != nil && !shouldSync() {
			// Keep the `uploaded` map fresh so re-enabling later
			// doesn't re-upload every modified file back through
			// "old state". Treat a disabled sync as if we had just
			// uploaded everything.
			entries, _ := os.ReadDir(taskPath)
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				if info, err := e.Info(); err == nil {
					if _, ok := uploaded[e.Name()]; !ok {
						uploaded[e.Name()] = &uploadedFile{
							modTime:        info.ModTime(),
							lastUploadTime: time.Now(),
						}
					} else {
						uploaded[e.Name()].modTime = info.ModTime()
					}
				}
			}
			return
		}
		f.uploadNewFiles(taskPath, channelID, threadTS, uploaded, &bundle)
	}

	for {
		select {
		case <-done:
			f.logger.Debug().Msg("output file watcher stopping")
			// Do one final check for new files.
			tick()
			return
		case <-ticker.C:
			tick()
		}
	}
}

// inlineSyncMaxBytes caps the size at which a file's content is
// posted inline as a code-block message (editable via chat.update)
// rather than uploaded as a file-share (not editable). Slack's
// chat.postMessage text limit is 40,000 chars; 16 KiB leaves ample
// headroom for the code fence, header, and any mrkdwn expansion
// while still covering the vast majority of scripts / configs /
// small JSON files that users tend to iterate on.
const inlineSyncMaxBytes = 16 * 1024

// uploadNewFiles checks for and uploads any new or modified files in the task directory.
//
// Two post paths:
//   - inline (small text, valid UTF-8): chat.postMessage with a code
//     block containing the full content. Editable via chat.update.
//     When the same file is re-synced and the bot hasn't posted
//     anything else to the thread since, the previous message is
//     updated in place rather than a new one being posted — this
//     stops iterative edits of the same script from flooding the
//     thread.
//   - file-share (binary or larger than inlineSyncMaxBytes): the
//     existing files.uploadV2 path. Slack's API doesn't let us
//     meaningfully edit a file-share message's attached content, so
//     this path always posts a new message.
func (f *FileHandler) uploadNewFiles(
	taskPath string,
	channelID string,
	threadTS string,
	uploaded map[string]*uploadedFile,
	bundle **outputBundle,
) {
	entries, err := os.ReadDir(taskPath)
	if err != nil {
		// Directory might not exist yet, that's ok.
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		localPath := filepath.Join(taskPath, name)

		// Get file info to check modification time.
		info1, err := entry.Info()
		if err != nil {
			continue
		}

		// Check if file should be uploaded (new or modified).
		tracked, exists := uploaded[name]
		shouldUpload := false

		if !exists {
			// New file - upload it.
			shouldUpload = true
		} else if info1.ModTime().After(tracked.modTime) {
			// File has been modified since last upload.
			// Apply cooldown period to prevent rapid re-uploads.
			cooldownPeriod := 10 * time.Second
			if time.Since(tracked.lastUploadTime) >= cooldownPeriod {
				shouldUpload = true
				f.logger.Debug().
					Str("file", name).
					Time("old_modtime", tracked.modTime).
					Time("new_modtime", info1.ModTime()).
					Msg("file modified, re-uploading")
			}
		}

		if !shouldUpload {
			continue
		}

		// Wait a moment and check again.
		time.Sleep(500 * time.Millisecond)
		info2, err := os.Stat(localPath)
		if err != nil {
			continue
		}

		if info1.Size() != info2.Size() {
			// File is still being written, skip for now.
			continue
		}

		// Read the file content so we can decide inline vs
		// file-upload and pass the bytes to the chosen path.
		content, readErr := os.ReadFile(localPath)
		if readErr != nil {
			f.logger.Warn().Err(readErr).Str("file", name).Msg("failed to read output file; skipping sync")
			continue
		}

		inlineCandidate := len(content) <= inlineSyncMaxBytes && isPrintableUTF8(content)

		if inlineCandidate {
			if f.syncInlineBundle(channelID, threadTS, name, content, bundle) {
				uploaded[name] = &uploadedFile{
					modTime:        info2.ModTime(),
					lastUploadTime: time.Now(),
				}
				continue
			}
			// Fall through to file-upload path on inline failure.
		}

		// Binary / large / inline-failed: use the file-upload path.
		if err := f.syncFileshareBundle(localPath, name, channelID, threadTS, bundle); err != nil {
			f.logger.Error().Err(err).Str("file", name).Msg("failed to upload output file")
			continue
		}
		uploaded[name] = &uploadedFile{
			modTime:        info2.ModTime(),
			lastUploadTime: time.Now(),
		}
	}
}

// inlineBundleMaxBytes caps the total serialized body of a
// consolidated inline bundle. Slack's chat.postMessage text limit is
// 40,000 chars; ~35 KiB leaves headroom for code fences, headers, and
// mrkdwn expansion across all entries in the bundle. When adding a
// new entry would exceed this, the current bundle is retired and a
// fresh one takes its place.
const inlineBundleMaxBytes = 35 * 1024

// syncInlineBundle posts or extends the consolidated inline output
// message for this thread. Returns true if the sync succeeded (bundle
// updated), false if it failed and the caller should fall back to the
// file-share path.
//
// Rules:
//   - If a bundle exists in the SAME mode, its message is still the
//     latest bot post, AND adding the new file keeps the body within
//     inlineBundleMaxBytes, extend it in place (chat.update).
//   - Otherwise start a fresh bundle: post a new message and reset
//     *bundlePtr.
//
// A re-sync of a file already in the bundle replaces its entry rather
// than adding a duplicate, so iterative edits stay tidy.
func (f *FileHandler) syncInlineBundle(
	channelID, threadTS, name string,
	content []byte,
	bundlePtr **outputBundle,
) bool {
	trimmed := trimTrailingNewlines(content)
	bundle := *bundlePtr

	// Extend path.
	if bundle != nil && bundle.mode == bundleModeInline && f.bot != nil &&
		f.bot.LatestPostTS(channelID, threadTS) == bundle.messageTS {
		nextEntries := replaceOrAppendInline(bundle.inline, name, trimmed)
		body := formatInlineBundleMessage(nextEntries)
		if len(body) <= inlineBundleMaxBytes {
			if err := f.bot.UpdateMessage(channelID, bundle.messageTS, body); err == nil {
				bundle.inline = nextEntries
				f.logger.Debug().
					Str("file", name).
					Int("entries", len(nextEntries)).
					Int("body_bytes", len(body)).
					Str("message_ts", bundle.messageTS).
					Msg("extended inline output bundle")
				return true
			} else {
				f.logger.Warn().Err(err).Str("file", name).Msg("inline bundle edit failed; falling back to post-new")
			}
		} else {
			f.logger.Debug().
				Str("file", name).
				Int("body_bytes", len(body)).
				Msg("inline bundle body would exceed cap; starting fresh bundle")
		}
	}

	// Post-new path: fresh bundle with just this file.
	entries := []inlineEntry{{name: name, content: trimmed}}
	body := formatInlineBundleMessage(entries)
	ts, err := f.postText(channelID, threadTS, body)
	if err != nil {
		f.logger.Error().Err(err).Str("file", name).Msg("failed to post inline sync")
		return false
	}
	*bundlePtr = &outputBundle{
		messageTS: ts,
		mode:      bundleModeInline,
		inline:    entries,
	}
	return true
}

// syncFileshareBundle uploads a binary/large file via the staged
// files.getUploadURLExternal → PUT → files.completeUploadExternal
// path, consolidating with prior fileshare syncs when possible.
//
// Rules:
//   - If a fileshare bundle exists and its message is still the
//     latest bot post, delete that message and re-share all
//     accumulated files (previous + new) via a single
//     completeUploadExternal call, producing one fresh message.
//   - Otherwise start a fresh bundle: share just this file.
//
// The delete + repost has visible flicker but produces one Slack
// message containing every file in the bundle, matching the
// inline-consolidation semantics.
func (f *FileHandler) syncFileshareBundle(
	localPath, name, channelID, threadTS string,
	bundlePtr **outputBundle,
) error {
	info, err := os.Stat(localPath)
	if err != nil {
		return oops.Trace(err)
	}
	fileID, err := f.uploadAndGetFileID(localPath, name, int(info.Size()))
	if err != nil {
		return oops.Trace(err)
	}
	newEntry := fileshareEntry{
		name:   name,
		fileID: fileID,
		title:  name,
	}

	bundle := *bundlePtr

	// Extend path: consolidate into one refreshed message.
	if bundle != nil && bundle.mode == bundleModeFileshare && f.bot != nil &&
		f.bot.LatestPostTS(channelID, threadTS) == bundle.messageTS {
		allEntries := append(append([]fileshareEntry{}, bundle.fileshare...), newEntry)
		if err := f.bot.DeleteMessage(channelID, bundle.messageTS); err != nil {
			f.logger.Warn().Err(err).Str("message_ts", bundle.messageTS).Msg("failed to delete prior fileshare bundle; posting fresh anyway")
		}
		ts, err := f.completeShareForBundle(channelID, threadTS, allEntries)
		if err != nil {
			return oops.Trace(err)
		}
		*bundlePtr = &outputBundle{
			messageTS: ts,
			mode:      bundleModeFileshare,
			fileshare: allEntries,
		}
		f.logger.Debug().
			Str("file", name).
			Int("entries", len(allEntries)).
			Str("message_ts", ts).
			Msg("extended fileshare output bundle")
		return nil
	}

	// Post-new path: fresh bundle with just this file.
	ts, err := f.completeShareForBundle(channelID, threadTS, []fileshareEntry{newEntry})
	if err != nil {
		return oops.Trace(err)
	}
	*bundlePtr = &outputBundle{
		messageTS: ts,
		mode:      bundleModeFileshare,
		fileshare: []fileshareEntry{newEntry},
	}
	return nil
}

// uploadAndGetFileID runs the two-step external upload (get URL, PUT
// bytes) and returns the file ID without sharing to any channel. The
// caller shares via completeUploadExternal when it's ready to compose
// the final message.
func (f *FileHandler) uploadAndGetFileID(localPath, name string, size int) (string, error) {
	u, err := f.client.GetUploadURLExternalContext(context.Background(), slack.GetUploadURLExternalParameters{
		FileName: name,
		FileSize: size,
	})
	if err != nil {
		return "", oops.Trace(err)
	}
	if err := f.client.UploadToURL(context.Background(), slack.UploadToURLParameters{
		UploadURL: u.UploadURL,
		File:      localPath,
		Filename:  name,
	}); err != nil {
		return "", oops.Trace(err)
	}
	return u.FileID, nil
}

// completeShareForBundle calls files.completeUploadExternal with all
// entries in the bundle, producing a single Slack message that shows
// every file. Returns the new message's TS.
func (f *FileHandler) completeShareForBundle(
	channelID, threadTS string,
	entries []fileshareEntry,
) (string, error) {
	files := make([]slack.FileSummary, 0, len(entries))
	for _, e := range entries {
		files = append(files, slack.FileSummary{ID: e.fileID, Title: e.title})
	}
	comment := formatFileshareComment(entries)
	resp, err := f.client.CompleteUploadExternalContext(context.Background(), slack.CompleteUploadExternalParameters{
		Files:           files,
		Channel:         channelID,
		ThreadTimestamp: threadTS,
		InitialComment:  comment,
	})
	if err != nil {
		return "", oops.Trace(err)
	}
	// Slack doesn't return the message TS from completeUploadExternal
	// directly, so we look it up via LatestPostTS after a short delay
	// to let Slack process. In practice the recordPost hook fires
	// from PostMessage paths — file-share doesn't hit that. Instead,
	// find the message via conversations.history.
	ts := f.findLatestBotMessage(channelID, threadTS, len(files))
	if ts != "" && f.bot != nil {
		f.bot.recordPost(channelID, threadTS, ts)
	}
	_ = resp
	return ts, nil
}

// findLatestBotMessage scans the recent thread history for the bot's
// most recent post that has attached files matching numFiles. Used
// to recover the message TS from completeUploadExternal (which
// doesn't return it).
func (f *FileHandler) findLatestBotMessage(channelID, threadTS string, numFiles int) string {
	if f.bot == nil {
		return ""
	}
	botUser, err := f.bot.UserID()
	if err != nil || botUser == "" {
		return ""
	}
	// Small delay so Slack has finished ingesting the share.
	time.Sleep(300 * time.Millisecond)
	params := &slack.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: threadTS,
		Limit:     20,
	}
	msgs, _, _, err := f.client.GetConversationReplies(params)
	if err != nil {
		f.logger.Debug().Err(err).Msg("failed to locate fileshare message TS after upload")
		return ""
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.User != botUser && m.BotID == "" {
			continue
		}
		if len(m.Files) >= numFiles {
			return m.Timestamp
		}
	}
	return ""
}

// postText sends a chat.postMessage with the given body, threading if
// threadTS is set. Uses the Bot wrapper when available so LatestPostTS
// tracking is maintained.
func (f *FileHandler) postText(channelID, threadTS, body string) (string, error) {
	if f.bot != nil {
		return f.bot.PostMessage(channelID, body, threadTS)
	}
	opts := []slack.MsgOption{slack.MsgOptionText(body, false)}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	_, ts, err := f.client.PostMessage(channelID, opts...)
	if err != nil {
		return "", oops.Trace(err)
	}
	return ts, nil
}

// replaceOrAppendInline returns a copy of entries with `name`'s entry
// replaced by (name, content). When `name` isn't already in entries
// the new record is appended.
func replaceOrAppendInline(entries []inlineEntry, name string, content []byte) []inlineEntry {
	out := make([]inlineEntry, 0, len(entries)+1)
	found := false
	for _, e := range entries {
		if e.name == name {
			out = append(out, inlineEntry{name: name, content: content})
			found = true
		} else {
			out = append(out, e)
		}
	}
	if !found {
		out = append(out, inlineEntry{name: name, content: content})
	}
	return out
}

// trimTrailingNewlines strips \n and \r from the end of content so the
// closing code fence sits flush.
func trimTrailingNewlines(content []byte) []byte {
	end := len(content)
	for end > 0 && (content[end-1] == '\n' || content[end-1] == '\r') {
		end--
	}
	return content[:end]
}

// formatInlineBundleMessage renders every entry as a
// `Output: name\n```content```` block joined by blank lines, so
// multiple files stay visually separated inside one mrkdwn message.
func formatInlineBundleMessage(entries []inlineEntry) string {
	if len(entries) == 0 {
		return ""
	}
	var b bytes.Buffer
	for i, e := range entries {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(":outbox_tray: Output: `")
		b.WriteString(e.name)
		b.WriteString("`\n```\n")
		b.Write(e.content)
		b.WriteString("\n```")
	}
	return b.String()
}

// formatFileshareComment renders the InitialComment shown above the
// file previews in a fileshare bundle. Single-file bundles read like
// the pre-consolidation post; multi-file bundles list every filename
// so readers can tell what's attached without expanding each preview.
func formatFileshareComment(entries []fileshareEntry) string {
	if len(entries) == 1 {
		return fmt.Sprintf(":outbox_tray: Output: `%s`", entries[0].name)
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, ":outbox_tray: Output (%d files):", len(entries))
	for _, e := range entries {
		b.WriteString("\n• `")
		b.WriteString(e.name)
		b.WriteString("`")
	}
	return b.String()
}

// isPrintableUTF8 reports whether b looks like printable text. We
// require valid UTF-8 and the absence of control bytes (except the
// common whitespace ones) so that picking "inline" for a file we
// then render with raw bytes doesn't produce a broken code block.
func isPrintableUTF8(b []byte) bool {
	for _, c := range b {
		switch {
		case c == 0:
			return false
		case c == '\t' || c == '\n' || c == '\r':
			continue
		case c < 0x20:
			return false
		}
	}
	return utf8.Valid(b)
}
