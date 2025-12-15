package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/calebcase/oops"
)

// SessionMapping represents a Slack thread to clod session mapping.
type SessionMapping struct {
	ChannelID string    `json:"channel_id"`
	ThreadTS  string    `json:"thread_ts"`
	TaskName  string    `json:"task_name"`
	TaskPath  string    `json:"task_path"`
	SessionID string    `json:"session_id"`
	UserID    string    `json:"user_id"`
	Verbose   bool      `json:"verbose"` // Per-thread verbosity setting
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SessionStore manages thread-to-session mappings with JSON persistence.
type SessionStore struct {
	path     string
	sessions map[string]*SessionMapping // key: "channelID:threadTS"
	mu       sync.RWMutex
}

// Count returns the number of stored sessions.
func (s *SessionStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}

// NewSessionStore creates a new SessionStore and loads existing sessions.
func NewSessionStore(path string) (*SessionStore, error) {
	s := &SessionStore{
		path:     path,
		sessions: make(map[string]*SessionMapping),
	}

	if err := s.Load(); err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	return s, nil
}

// key generates the map key for a channel/thread pair.
func key(channelID, threadTS string) string {
	return channelID + ":" + threadTS
}

// Get retrieves a session mapping by channel and thread.
func (s *SessionStore) Get(channelID, threadTS string) *SessionMapping {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.sessions[key(channelID, threadTS)]
}

// Set stores a session mapping.
func (s *SessionStore) Set(mapping *SessionMapping) {
	s.mu.Lock()
	defer s.mu.Unlock()

	mapping.UpdatedAt = time.Now()
	s.sessions[key(mapping.ChannelID, mapping.ThreadTS)] = mapping
}

// SetVerbose updates the verbose setting for a thread.
// If no session exists, it creates a minimal one to store the setting.
func (s *SessionStore) SetVerbose(channelID, threadTS string, verbose bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	k := key(channelID, threadTS)
	session := s.sessions[k]
	if session == nil {
		// Create minimal session to track verbosity for threads without tasks yet
		session = &SessionMapping{
			ChannelID: channelID,
			ThreadTS:  threadTS,
			CreatedAt: time.Now(),
		}
		s.sessions[k] = session
	}
	session.Verbose = verbose
	session.UpdatedAt = time.Now()
}

// IsVerbose returns the verbosity setting for a thread.
// Returns false (quiet mode) if no session exists.
func (s *SessionStore) IsVerbose(channelID, threadTS string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	session := s.sessions[key(channelID, threadTS)]
	if session == nil {
		return false
	}
	return session.Verbose
}

// Load reads sessions from the JSON file.
// Returns nil if the file doesn't exist (fresh start).
func (s *SessionStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			// No sessions file yet, start fresh
			return nil
		}
		return oops.Trace(err)
	}

	var sessions []*SessionMapping
	if err := json.Unmarshal(data, &sessions); err != nil {
		return oops.Trace(err)
	}

	s.sessions = make(map[string]*SessionMapping, len(sessions))
	for _, session := range sessions {
		s.sessions[key(session.ChannelID, session.ThreadTS)] = session
	}

	return nil
}

// Save writes sessions to the JSON file atomically.
func (s *SessionStore) Save() error {
	s.mu.RLock()
	sessions := make([]*SessionMapping, 0, len(s.sessions))
	for _, session := range s.sessions {
		sessions = append(sessions, session)
	}
	s.mu.RUnlock()

	data, err := json.MarshalIndent(sessions, "", "  ")
	if err != nil {
		return oops.Trace(err)
	}

	// Atomic write: temp file + rename
	dir := filepath.Dir(s.path)
	tmpFile, err := os.CreateTemp(dir, "sessions-*.json.tmp")
	if err != nil {
		return oops.Trace(err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return oops.Trace(err)
	}

	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return oops.Trace(err)
	}

	if err := os.Rename(tmpPath, s.path); err != nil {
		_ = os.Remove(tmpPath)
		return oops.Trace(err)
	}

	return nil
}
