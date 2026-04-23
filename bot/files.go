package main

import (
	"bytes"
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
	// messageTS is the Slack ts of the most recent sync post for
	// this file. Populated only for inline-text syncs (code-block
	// chat.postMessage); empty for file-upload path since
	// UploadFileV2 doesn't give us a usable message TS and
	// chat.update can't replace file-attached content anyway.
	messageTS string
	// inlineText records whether the last sync for this file was
	// posted inline (chat.postMessage + code block) so the next
	// sync can decide whether an in-place edit is possible.
	inlineText bool
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
		f.uploadNewFiles(taskPath, channelID, threadTS, uploaded)
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
			messageTS, usedEdit := f.syncInline(channelID, threadTS, name, content, tracked)
			if messageTS != "" {
				uploaded[name] = &uploadedFile{
					modTime:        info2.ModTime(),
					lastUploadTime: time.Now(),
					messageTS:      messageTS,
					inlineText:     true,
				}
				f.logger.Debug().
					Str("file", name).
					Bool("edited", usedEdit).
					Str("message_ts", messageTS).
					Msg("inline file sync posted")
				continue
			}
			// Fall through to file-upload path on inline failure.
		}

		// Binary / large / inline-failed: use the file-upload path.
		_, err = f.UploadFromTaskOutputs(localPath, channelID, threadTS, fmt.Sprintf(":outbox_tray: Output: `%s`", name))
		if err != nil {
			f.logger.Error().Err(err).Str("file", name).Msg("failed to upload output file")
			continue
		}
		uploaded[name] = &uploadedFile{
			modTime:        info2.ModTime(),
			lastUploadTime: time.Now(),
		}
	}
}

// syncInline posts (or edits) a code-block message carrying the file's
// content. Edits only when the bot hasn't posted anything else on
// this thread since the previous inline sync message for this file —
// otherwise posts a new message so the update lands below the
// interleaved activity. Returns the resolved Slack message TS and
// whether an in-place edit was used.
func (f *FileHandler) syncInline(
	channelID, threadTS, name string,
	content []byte,
	tracked *uploadedFile,
) (string, bool) {
	body := formatInlineSyncMessage(name, content)

	// Edit path: previous inline sync message exists, and the thread's
	// latest bot post is still that message.
	if tracked != nil && tracked.inlineText && tracked.messageTS != "" && f.bot != nil {
		if f.bot.LatestPostTS(channelID, threadTS) == tracked.messageTS {
			if err := f.bot.UpdateMessage(channelID, tracked.messageTS, body); err == nil {
				return tracked.messageTS, true
			} else {
				f.logger.Warn().Err(err).Str("file", name).Msg("inline sync edit failed; falling back to post-new")
			}
		}
	}

	// Post-new path.
	if f.bot == nil {
		// AttachBot wasn't wired; posting falls back to direct client call.
		opts := []slack.MsgOption{slack.MsgOptionText(body, false)}
		if threadTS != "" {
			opts = append(opts, slack.MsgOptionTS(threadTS))
		}
		_, ts, err := f.client.PostMessage(channelID, opts...)
		if err != nil {
			f.logger.Error().Err(err).Str("file", name).Msg("failed to post inline sync (no bot)")
			return "", false
		}
		return ts, false
	}
	ts, err := f.bot.PostMessage(channelID, body, threadTS)
	if err != nil {
		f.logger.Error().Err(err).Str("file", name).Msg("failed to post inline sync")
		return "", false
	}
	return ts, false
}

// formatInlineSyncMessage renders `name` + content as a mrkdwn
// message with a code block. Tabs inside content are preserved;
// trailing whitespace is trimmed so the closing fence stays flush.
func formatInlineSyncMessage(name string, content []byte) string {
	text := string(content)
	// Trim only trailing newlines so the closing ``` sits on its
	// own line without extra blank padding.
	for len(text) > 0 && (text[len(text)-1] == '\n' || text[len(text)-1] == '\r') {
		text = text[:len(text)-1]
	}
	return fmt.Sprintf(":outbox_tray: Output: `%s`\n```\n%s\n```", name, text)
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
