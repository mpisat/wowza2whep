package main

import "strings"

// WowzaGetOfferRequest asks Wowza to send its SDP offer for playback
type WowzaGetOfferRequest struct {
	Direction   string            `json:"direction"`
	Command     string            `json:"command"`
	StreamInfo  WowzaStreamInfo   `json:"streamInfo"`
	UserData    map[string]string `json:"userData,omitempty"`
	SecureToken *string           `json:"secureToken"`
}

// WowzaSendResponseRequest sends our SDP answer to Wowza
type WowzaSendResponseRequest struct {
	Direction  string            `json:"direction"`
	Command    string            `json:"command"`
	StreamInfo WowzaStreamInfo   `json:"streamInfo"`
	SDP        WowzaSDP          `json:"sdp"`
	UserData   map[string]string `json:"userData,omitempty"`
}

type WowzaStreamInfo struct {
	ApplicationName string `json:"applicationName"`
	StreamName      string `json:"streamName"`
	SessionID       string `json:"sessionId"`
}

// WowzaICECandidate represents an ICE candidate from Wowza
type WowzaICECandidate struct {
	Candidate     string  `json:"candidate"`
	SDPMid        *string `json:"sdpMid"`
	SDPMLineIndex *uint16 `json:"sdpMLineIndex"`
}

// WowzaResponse is Wowza's response to our requests
type WowzaResponse struct {
	Status            int                 `json:"status"`
	StatusDescription string              `json:"statusDescription,omitempty"`
	Direction         string              `json:"direction,omitempty"`
	Command           string              `json:"command,omitempty"`
	StreamInfo        WowzaStreamInfo     `json:"streamInfo,omitempty"`
	SDP               *WowzaSDP           `json:"sdp,omitempty"`
	ICECandidates     []WowzaICECandidate `json:"iceCandidates,omitempty"`
}

type WowzaSDP struct {
	SDP  string `json:"sdp"`
	Type string `json:"type,omitempty"`
}

// cleanWowzaCandidate fixes Wowza Cloud candidate format issues
func cleanWowzaCandidate(candidate string) string {
	// Remove "generation X" suffix (non-standard)
	if idx := strings.Index(candidate, " generation"); idx > 0 {
		candidate = strings.TrimSpace(candidate[:idx])
	}

	// Add tcptype passive for TCP candidates (RFC 6544 requirement)
	parts := strings.Fields(candidate)
	if len(parts) >= 3 {
		transport := strings.ToUpper(parts[2])
		if transport == "TCP" && !strings.Contains(candidate, "tcptype") {
			candidate = candidate + " tcptype passive"
		}
	}

	return candidate
}
