package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Manager handles session lifecycle.
type Manager struct {
	cfg    *Config
	logger *slog.Logger

	mu       sync.RWMutex
	sessions map[string]*Session
}

// NewManager creates a new session manager.
func NewManager(cfg *Config, logger *slog.Logger) *Manager {
	return &Manager{
		cfg:      cfg,
		logger:   logger,
		sessions: make(map[string]*Session),
	}
}

// Create returns a new signaling session.
func (m *Manager) Create(appName, streamName, wsURL string) (string, *Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := "session-" + uuid.New().String()
	sess := NewSession(id, appName, streamName, wsURL, m.cfg, m.logger)
	sess.SetStopCallback(m.onSessionStopped)
	m.sessions[id] = sess

	m.logger.Info("session created",
		"session_id", id,
		"app", appName,
		"stream", streamName,
		"active", len(m.sessions),
	)

	return id, sess, nil
}

func (m *Manager) onSessionStopped(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	count := len(m.sessions)
	m.mu.Unlock()

	m.logger.Info("session removed", "session_id", id, "active", count)
}

// Get retrieves a session by ID.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sess, ok := m.sessions[id]
	return sess, ok
}

// Remove stops and removes a session.
func (m *Manager) Remove(id string) {
	m.mu.Lock()
	sess, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()

	if ok {
		go sess.Stop()
	}
}

// ActiveIDs returns all active session IDs.
func (m *Manager) ActiveIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		out = append(out, id)
	}
	return out
}

// Stats returns statistics for all sessions.
func (m *Manager) Stats() map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sessions := make([]map[string]any, 0, len(m.sessions))
	for _, sess := range m.sessions {
		sessions = append(sessions, sess.Stats())
	}
	return map[string]any{
		"active_sessions": len(m.sessions),
		"timestamp":       time.Now().Unix(),
		"sessions":        sessions,
	}
}

// Shutdown gracefully stops all sessions.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	snapshot := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		snapshot = append(snapshot, s)
	}
	m.sessions = make(map[string]*Session)
	m.mu.Unlock()

	if len(snapshot) == 0 {
		return nil
	}

	m.logger.Info("shutting down sessions", "count", len(snapshot))

	var wg sync.WaitGroup
	for _, s := range snapshot {
		wg.Add(1)
		go func(sess *Session) {
			defer wg.Done()
			sess.Stop()
		}(s)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
