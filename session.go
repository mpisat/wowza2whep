package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Session bridges WHEP client and Wowza signaling. WebSocket closes after SDP exchange.
type Session struct {
	id         string
	appName    string
	streamName string
	wsURL      string

	cfg    *Config
	logger *slog.Logger

	wowzaSessionID string
	createdAt      time.Time

	mu       sync.Mutex
	stopped  bool
	onStop   func(string)
	stopOnce sync.Once
}

// NewSession creates a new signaling-only session.
func NewSession(id, appName, streamName, wsURL string, cfg *Config, logger *slog.Logger) *Session {
	return &Session{
		id:         id,
		appName:    appName,
		streamName: streamName,
		wsURL:      wsURL,
		cfg:        cfg,
		logger:     logger.With("session_id", id),
		createdAt:  time.Now(),
	}
}

func (s *Session) ID() string { return s.id }

func (s *Session) SetStopCallback(fn func(string)) { s.onStop = fn }

// Negotiate performs the WHEP signaling exchange with Wowza.
// Wowza's play protocol is inverted from WHEP: Wowza sends the SDP offer, we send the answer.
// We bridge this by creating two answers with swapped ICE/DTLS credentials.
func (s *Session) Negotiate(clientOffer string) (string, error) {
	timeout := s.cfg.WsTimeout
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	dialer := websocket.Dialer{
		HandshakeTimeout: timeout / 2,
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: s.cfg.InsecureTLS},
	}

	conn, _, err := dialer.DialContext(ctx, s.wsURL, nil)
	if err != nil {
		return "", fmt.Errorf("websocket dial: %w", err)
	}
	defer conn.Close()

	deadline := time.Now().Add(timeout)
	conn.SetReadDeadline(deadline)
	conn.SetWriteDeadline(deadline)

	// Step 1: Request offer from Wowza
	getOfferReq := WowzaGetOfferRequest{
		Direction: "play",
		Command:   "getOffer",
		StreamInfo: WowzaStreamInfo{
			ApplicationName: s.appName,
			StreamName:      s.streamName,
		},
	}

	if err := conn.WriteJSON(&getOfferReq); err != nil {
		return "", fmt.Errorf("send getOffer: %w", err)
	}

	// Step 2: Receive Wowza's offer
	var offerResp WowzaResponse
	if err := conn.ReadJSON(&offerResp); err != nil {
		return "", fmt.Errorf("read getOffer response: %w", err)
	}

	if offerResp.Status < 200 || offerResp.Status >= 300 {
		return "", fmt.Errorf("wowza error: %s", offerResp.StatusDescription)
	}

	if offerResp.SDP == nil || offerResp.SDP.SDP == "" {
		return "", fmt.Errorf("wowza returned empty SDP offer")
	}

	s.wowzaSessionID = offerResp.StreamInfo.SessionID
	s.logger.Info("received offer from Wowza", "wowza_session_id", s.wowzaSessionID)

	// Step 3: Create answer for Wowza with client's ICE/DTLS credentials
	answerForWowza, err := CreateAnswerForWowza(offerResp.SDP.SDP, clientOffer)
	if err != nil {
		return "", fmt.Errorf("create answer for wowza: %w", err)
	}

	// Step 4: Send answer to Wowza
	sendRespReq := WowzaSendResponseRequest{
		Direction: "play",
		Command:   "sendResponse",
		StreamInfo: WowzaStreamInfo{
			ApplicationName: s.appName,
			StreamName:      s.streamName,
			SessionID:       s.wowzaSessionID,
		},
		SDP: WowzaSDP{Type: "answer", SDP: answerForWowza},
	}

	if err := conn.WriteJSON(&sendRespReq); err != nil {
		return "", fmt.Errorf("send sendResponse: %w", err)
	}

	// Step 5: Receive ICE candidates from Wowza
	var candidatesResp WowzaResponse
	if err := conn.ReadJSON(&candidatesResp); err != nil {
		return "", fmt.Errorf("read sendResponse response: %w", err)
	}

	if candidatesResp.Status < 200 || candidatesResp.Status >= 300 {
		return "", fmt.Errorf("wowza error: %s", candidatesResp.StatusDescription)
	}

	s.logger.Info("signaling complete", "ice_candidates", len(candidatesResp.ICECandidates))

	// Step 6: Create answer for client with Wowza's ICE/DTLS credentials
	answerForClient, err := CreateAnswerForClient(offerResp.SDP.SDP, clientOffer, candidatesResp.ICECandidates)
	if err != nil {
		return "", fmt.Errorf("create answer for client: %w", err)
	}

	return answerForClient, nil
}

// AddICECandidate is a no-op; all candidates are in the initial SDP exchange.
func (s *Session) AddICECandidate(candidate string, sdpMid *string) error {
	s.logger.Debug("ignoring trickle ICE candidate", "candidate", candidate)
	return nil
}

// Stop marks the session as stopped and triggers cleanup callback.
func (s *Session) Stop() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()

		if s.onStop != nil {
			s.onStop(s.id)
		}
	})
}

func (s *Session) Stats() map[string]any {
	return map[string]any{
		"id":               s.id,
		"app":              s.appName,
		"stream":           s.streamName,
		"wowza_session_id": s.wowzaSessionID,
		"created_at":       s.createdAt.Unix(),
		"age_secs":         int(time.Since(s.createdAt).Seconds()),
	}
}
