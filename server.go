package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
)

type Server struct {
	cfg    *Config
	mgr    *Manager
	logger *slog.Logger
	server *http.Server
}

func NewServer(cfg *Config, mgr *Manager, logger *slog.Logger) *Server {
	return &Server{cfg: cfg, mgr: mgr, logger: logger}
}

// Start runs the HTTP server until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	mux.HandleFunc("/whep/", s.handleWHEP)
	mux.HandleFunc("/whep/cloud/", s.handleWHEPCloud)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/stats", s.handleStats)

	s.server = &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           s.withLogging(s.withCORS(mux)),
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	s.logger.Info("HTTP server started", "address", s.cfg.ListenAddr)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return s.Stop(shutdownCtx)
	}
}

// Stop gracefully shuts down the server.
func (s *Server) Stop(ctx context.Context) error {
	if s.server != nil {
		_ = s.server.Shutdown(ctx)
	}
	return s.mgr.Shutdown(ctx)
}

// Static mode: /whep/{codec}/{app}/{stream}
func (s *Server) handleWHEP(w http.ResponseWriter, r *http.Request) {
	if s.cfg.WowzaWSURL == "" {
		http.Error(w, "websocket URL not configured - use /whep/cloud/ or start with -websocket flag", http.StatusServiceUnavailable)
		return
	}

	urlPath := strings.TrimPrefix(r.URL.Path, "/whep/")
	urlPath = strings.TrimPrefix(urlPath, "/")

	if urlPath == "" {
		http.Error(w, "format: /whep/{codec}/{app}/{stream} where codec is h264 or vp8", http.StatusBadRequest)
		return
	}

	// Check for session operations (DELETE, PATCH)
	parts := strings.Split(urlPath, "/")
	if len(parts) > 0 && strings.HasPrefix(parts[len(parts)-1], "session-") {
		sessionID := parts[len(parts)-1]
		s.handleSessionOp(w, r, sessionID)
		return
	}

	// Parse codec from first path segment
	if len(parts) < 3 {
		http.Error(w, "format: /whep/{codec}/{app}/{stream} where codec is h264 or vp8", http.StatusBadRequest)
		return
	}

	codec := strings.ToLower(parts[0])
	if codec != "h264" && codec != "vp8" {
		http.Error(w, "codec must be h264 or vp8", http.StatusBadRequest)
		return
	}

	// Parse app/stream from remaining path
	remaining := strings.Join(parts[1:], "/")
	appName, streamName, err := parseAppStream(remaining)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodPost:
		s.handleCreate(w, r, appName, streamName, s.cfg.WowzaWSURL)
	case http.MethodOptions:
		s.writeWHEPOptions(w)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// Dynamic mode: /whep/cloud/{codec}/{host}/{app}/{stream}
func (s *Server) handleWHEPCloud(w http.ResponseWriter, r *http.Request) {
	urlPath := strings.TrimPrefix(r.URL.Path, "/whep/cloud/")
	urlPath = strings.TrimPrefix(urlPath, "/")

	if urlPath == "" {
		http.Error(w, "format: /whep/cloud/{codec}/{host}/{app}/{stream} where codec is h264 or vp8", http.StatusBadRequest)
		return
	}

	// Parse: {codec}/{host}/{app}/{stream} or {codec}/{host}/{app}/{stream}/session-xxx
	parts := strings.Split(urlPath, "/")

	// Check for session operations first (session ID is always last)
	if len(parts) > 0 && strings.HasPrefix(parts[len(parts)-1], "session-") {
		sessionID := parts[len(parts)-1]
		s.handleSessionOp(w, r, sessionID)
		return
	}

	// Need at least: codec/host/app/stream
	if len(parts) < 4 {
		http.Error(w, "format: /whep/cloud/{codec}/{host}/{app}/{stream} where codec is h264 or vp8", http.StatusBadRequest)
		return
	}

	codec := strings.ToLower(parts[0])
	if codec != "h264" && codec != "vp8" {
		http.Error(w, "codec must be h264 or vp8", http.StatusBadRequest)
		return
	}

	host := parts[1]

	// Validate host
	if !isValidHost(host) {
		http.Error(w, "invalid host", http.StatusBadRequest)
		return
	}

	// Check allowed hosts
	if !s.cfg.IsHostAllowed(host) {
		s.logger.Warn("host not allowed", "host", host)
		http.Error(w, "host not allowed", http.StatusForbidden)
		return
	}

	// Parse app/stream from remaining path (after codec and host)
	remaining := strings.Join(parts[2:], "/")
	appName, streamName, err := parseAppStream(remaining)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Build WebSocket URL
	var wsURL string
	if strings.Contains(host, ".") {
		// Full hostname (on-prem)
		wsURL = fmt.Sprintf("wss://%s/webrtc-session.json", host)
	} else {
		// Wowza Cloud ID
		wsURL = fmt.Sprintf("wss://%s.entrypoint.cloud.wowza.com/webrtc-session.json", host)
	}

	switch r.Method {
	case http.MethodPost:
		s.handleCreate(w, r, appName, streamName, wsURL)
	case http.MethodOptions:
		s.writeWHEPOptions(w)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request, appName, streamName, wsURL string) {
	offer, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		http.Error(w, "failed to read offer", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	if len(offer) == 0 {
		http.Error(w, "empty SDP offer", http.StatusBadRequest)
		return
	}

	contentType := r.Header.Get("Content-Type")
	if !strings.Contains(contentType, "application/sdp") {
		http.Error(w, "Content-Type must be application/sdp", http.StatusUnsupportedMediaType)
		return
	}

	s.logger.Info("WHEP create request",
		"app", appName,
		"stream", streamName,
		"user_agent", r.Header.Get("User-Agent"),
	)

	sessionID, session, err := s.mgr.Create(appName, streamName, wsURL)
	if err != nil {
		s.logger.Error("failed to create session", "error", err)
		http.Error(w, "failed to create session", http.StatusInternalServerError)
		return
	}

	answer, err := session.Negotiate(string(offer))
	if err != nil {
		s.logger.Error("signaling failed", "session_id", sessionID, "error", err)
		s.mgr.Remove(sessionID)

		status := http.StatusBadGateway
		msg := "signaling failed"
		if strings.Contains(err.Error(), "wowza error") {
			msg = err.Error()
		}
		http.Error(w, msg, status)
		return
	}

	s.logger.Debug("SDP answer", "sdp", answer)

	resourcePath := path.Join(r.URL.Path, sessionID)
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", resourcePath)
	w.Header().Set("Accept-Patch", "application/trickle-ice-sdpfrag")
	w.Header().Set("Link", fmt.Sprintf("<%s>; rel=\"ice-server\"", resourcePath))

	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(answer))

	s.logger.Info("WHEP session created",
		"session_id", sessionID,
		"app", appName,
		"stream", streamName,
	)
}

func (s *Server) handleSessionOp(w http.ResponseWriter, r *http.Request, sessionID string) {
	session, ok := s.mgr.Get(sessionID)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodPatch:
		// Trickle ICE - add ICE candidate
		s.handleICECandidate(w, r, session)
	case http.MethodDelete:
		s.mgr.Remove(sessionID)
		w.WriteHeader(http.StatusOK)
	case http.MethodOptions:
		s.writeWHEPOptions(w)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleICECandidate(w http.ResponseWriter, r *http.Request, session *Session) {
	contentType := r.Header.Get("Content-Type")
	if !strings.Contains(contentType, "application/trickle-ice-sdpfrag") {
		http.Error(w, "Content-Type must be application/trickle-ice-sdpfrag", http.StatusUnsupportedMediaType)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 4*1024))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	candidate, sdpMid := parseICEFragment(string(body))
	if candidate == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if err := session.AddICECandidate(candidate, sdpMid); err != nil {
		s.logger.Error("failed to add ICE candidate", "error", err)
		http.Error(w, "failed to add ICE candidate", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func parseICEFragment(frag string) (candidate string, sdpMid *string) {
	lines := strings.Split(frag, "\r\n")
	if len(lines) == 1 {
		lines = strings.Split(frag, "\n")
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "a=candidate:") {
			candidate = strings.TrimPrefix(line, "a=")
		} else if strings.HasPrefix(line, "a=mid:") {
			mid := strings.TrimPrefix(line, "a=mid:")
			sdpMid = &mid
		}
	}

	return candidate, sdpMid
}

func (s *Server) writeWHEPOptions(w http.ResponseWriter) {
	w.Header().Set("Accept-Post", "application/sdp")
	w.Header().Set("Accept-Patch", "application/trickle-ice-sdpfrag")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := map[string]any{
		"status":          "healthy",
		"active_sessions": len(s.mgr.ActiveIDs()),
		"timestamp":       time.Now().Unix(),
		"version":         Version,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.mgr.Stats())
}

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Expose-Headers", "Location, Link, Accept-Patch")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)

		if r.URL.Path == "/health" {
			return
		}

		s.logger.Info("HTTP request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration", time.Since(start).String(),
		)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// parseAppStream parses "app/stream" from URL path
func parseAppStream(urlPath string) (appName, streamName string, err error) {
	parts := strings.Split(urlPath, "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("format: {app}/{stream}")
	}

	appName = parts[0]
	streamName = strings.Join(parts[1:], "/")

	if err := validatePathSegment(appName); err != nil {
		return "", "", fmt.Errorf("invalid app name: %w", err)
	}

	// Stream name can contain query params (token)
	streamBase := strings.Split(streamName, "?")[0]
	if err := validatePathSegment(streamBase); err != nil {
		return "", "", fmt.Errorf("invalid stream name: %w", err)
	}

	return appName, streamName, nil
}

var pathSegmentRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func validatePathSegment(seg string) error {
	if seg == "" {
		return fmt.Errorf("empty segment")
	}
	if seg == "." || seg == ".." {
		return fmt.Errorf("invalid segment")
	}
	if !pathSegmentRe.MatchString(seg) {
		return fmt.Errorf("invalid characters")
	}
	return nil
}

// isValidHost validates host format
func isValidHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	for _, r := range host {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-') {
			return false
		}
	}
	if strings.Contains(host, "..") || host[0] == '.' || host[0] == '-' || host[len(host)-1] == '.' || host[len(host)-1] == '-' {
		return false
	}
	return true
}
