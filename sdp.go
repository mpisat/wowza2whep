package main

import (
	"fmt"
	"net"
	"strings"

	"github.com/pion/sdp/v3"
)

// ICECredentials holds ICE and DTLS credentials extracted from an SDP
type ICECredentials struct {
	IceUfrag    string
	IcePwd      string
	Fingerprint string
	Setup       string
	Candidates  []string
}

// MediaInfo holds information about a media section
type MediaInfo struct {
	Mid  string
	Type string // "video" or "audio"
}

// splitSDPLines splits SDP by CRLF or LF
func splitSDPLines(sdp string) []string {
	lines := strings.Split(sdp, "\r\n")
	if len(lines) == 1 {
		lines = strings.Split(sdp, "\n")
	}
	return lines
}

// ExtractCredentials extracts ICE/DTLS credentials from an SDP
func ExtractCredentials(sdpStr string) (*ICECredentials, error) {
	creds := &ICECredentials{}

	for _, line := range splitSDPLines(sdpStr) {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "a=ice-ufrag:"):
			creds.IceUfrag = strings.TrimPrefix(line, "a=ice-ufrag:")
		case strings.HasPrefix(line, "a=ice-pwd:"):
			creds.IcePwd = strings.TrimPrefix(line, "a=ice-pwd:")
		case strings.HasPrefix(line, "a=fingerprint:"):
			creds.Fingerprint = strings.TrimPrefix(line, "a=fingerprint:")
		case strings.HasPrefix(line, "a=setup:"):
			creds.Setup = strings.TrimPrefix(line, "a=setup:")
		case strings.HasPrefix(line, "a=candidate:"):
			creds.Candidates = append(creds.Candidates, strings.TrimPrefix(line, "a="))
		}
	}

	return creds, nil
}

// ExtractMediaOrder extracts the order and mid values of media sections from an SDP
func ExtractMediaOrder(sdpStr string) []MediaInfo {
	var result []MediaInfo
	var current *MediaInfo

	for _, line := range splitSDPLines(sdpStr) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "m=") {
			if current != nil {
				result = append(result, *current)
			}
			parts := strings.Fields(line)
			if len(parts) >= 4 {
				current = &MediaInfo{Type: parts[0][2:]}
			}
		} else if current != nil && strings.HasPrefix(line, "a=mid:") {
			current.Mid = strings.TrimPrefix(line, "a=mid:")
		}
	}
	if current != nil {
		result = append(result, *current)
	}

	return result
}

// CreateAnswerForWowza creates an SDP answer for Wowza using the client's ICE/DTLS credentials.
// This allows Wowza to connect directly to the client for media flow.
func CreateAnswerForWowza(wowzaOffer, clientOffer string) (string, error) {
	clientCreds, err := ExtractCredentials(clientOffer)
	if err != nil {
		return "", fmt.Errorf("extract client credentials: %w", err)
	}

	var wowzaDesc sdp.SessionDescription
	if err := wowzaDesc.Unmarshal([]byte(wowzaOffer)); err != nil {
		return "", fmt.Errorf("parse wowza offer: %w", err)
	}

	answerDesc := wowzaDesc

	// Replace session-level fingerprint
	for i, attr := range answerDesc.Attributes {
		if attr.Key == "fingerprint" && clientCreds.Fingerprint != "" {
			answerDesc.Attributes[i] = sdp.Attribute{Key: "fingerprint", Value: clientCreds.Fingerprint}
		}
	}

	// Update each media section with client's ICE/DTLS credentials
	for _, md := range answerDesc.MediaDescriptions {
		filtered := make([]sdp.Attribute, 0, len(md.Attributes))
		for _, attr := range md.Attributes {
			switch attr.Key {
			case "ice-ufrag":
				if clientCreds.IceUfrag != "" {
					filtered = append(filtered, sdp.Attribute{Key: "ice-ufrag", Value: clientCreds.IceUfrag})
				}
			case "ice-pwd":
				if clientCreds.IcePwd != "" {
					filtered = append(filtered, sdp.Attribute{Key: "ice-pwd", Value: clientCreds.IcePwd})
				}
			case "fingerprint":
				if clientCreds.Fingerprint != "" {
					filtered = append(filtered, sdp.Attribute{Key: "fingerprint", Value: clientCreds.Fingerprint})
				}
			case "setup":
				filtered = append(filtered, sdp.Attribute{Key: "setup", Value: "active"})
			case "sendrecv":
				filtered = append(filtered, sdp.Attribute{Key: "recvonly", Value: ""})
			case "candidate":
				continue // Skip Wowza's candidates
			default:
				filtered = append(filtered, attr)
			}
		}

		// Add client's ICE candidates
		for _, cand := range clientCreds.Candidates {
			filtered = append(filtered, sdp.Attribute{Key: "candidate", Value: cand})
		}

		md.Attributes = filtered
	}

	bytes, err := answerDesc.Marshal()
	if err != nil {
		return "", fmt.Errorf("marshal answer: %w", err)
	}

	result := string(bytes)
	result = filterPrivateIPs(result)
	result = addTrickleICE(result)

	return result, nil
}

// CreateAnswerForClient creates an SDP answer for the WHEP client using Wowza's ICE/DTLS credentials.
// The answer matches the client's offer structure (mid values, m-line order) but uses Wowza's
// credentials and payload types for direct client-to-Wowza media flow.
func CreateAnswerForClient(wowzaOffer, clientOffer string, wowzaCandidates []WowzaICECandidate) (string, error) {
	clientMedia := ExtractMediaOrder(clientOffer)

	wowzaCreds, err := ExtractCredentials(wowzaOffer)
	if err != nil {
		return "", fmt.Errorf("extract wowza credentials: %w", err)
	}

	if wowzaCreds.Fingerprint == "" {
		return "", fmt.Errorf("wowza offer missing fingerprint")
	}

	var wowzaDesc sdp.SessionDescription
	if err := wowzaDesc.Unmarshal([]byte(wowzaOffer)); err != nil {
		return "", fmt.Errorf("parse wowza offer: %w", err)
	}

	// Build map of Wowza's media sections by type
	wowzaMediaByType := make(map[string]*sdp.MediaDescription)
	for _, md := range wowzaDesc.MediaDescriptions {
		wowzaMediaByType[strings.ToLower(md.MediaName.Media)] = md
	}

	// Create answer with client's structure but Wowza's credentials
	answerDesc := sdp.SessionDescription{
		Version: 0,
		Origin: sdp.Origin{
			Username:       "-",
			SessionID:      wowzaDesc.Origin.SessionID,
			SessionVersion: wowzaDesc.Origin.SessionVersion,
			NetworkType:    "IN",
			AddressType:    "IP4",
			UnicastAddress: "127.0.0.1",
		},
		SessionName: "-",
		TimeDescriptions: []sdp.TimeDescription{
			{Timing: sdp.Timing{StartTime: 0, StopTime: 0}},
		},
	}

	// Build BUNDLE group with client's mid values
	var bundleMids []string
	for _, cm := range clientMedia {
		bundleMids = append(bundleMids, cm.Mid)
	}

	answerDesc.Attributes = []sdp.Attribute{
		{Key: "group", Value: "BUNDLE " + strings.Join(bundleMids, " ")},
		{Key: "msid-semantic", Value: "WMS *"},
		{Key: "fingerprint", Value: wowzaCreds.Fingerprint},
	}

	// Build media sections in client's order
	for i, clientMediaInfo := range clientMedia {
		mediaType := strings.ToLower(clientMediaInfo.Type)
		wowzaMD, ok := wowzaMediaByType[mediaType]

		if !ok {
			// Reject media type not available from Wowza
			md := &sdp.MediaDescription{
				MediaName: sdp.MediaName{
					Media:   mediaType,
					Port:    sdp.RangedPort{Value: 0},
					Protos:  []string{"UDP", "TLS", "RTP", "SAVPF"},
					Formats: []string{"0"},
				},
			}
			md.Attributes = []sdp.Attribute{
				{Key: "mid", Value: clientMediaInfo.Mid},
				{Key: "ice-ufrag", Value: wowzaCreds.IceUfrag},
				{Key: "ice-pwd", Value: wowzaCreds.IcePwd},
				{Key: "fingerprint", Value: wowzaCreds.Fingerprint},
				{Key: "setup", Value: "passive"},
				{Key: "inactive", Value: ""},
			}
			answerDesc.MediaDescriptions = append(answerDesc.MediaDescriptions, md)
			continue
		}

		// Use Wowza's payload types directly - in signaling-only mode we don't rewrite
		// RTP packets, so browser must be prepared to receive Wowza's fixed PTs
		md := &sdp.MediaDescription{
			MediaName: sdp.MediaName{
				Media:   wowzaMD.MediaName.Media,
				Port:    sdp.RangedPort{Value: 9},
				Protos:  []string{"UDP", "TLS", "RTP", "SAVPF"},
				Formats: wowzaMD.MediaName.Formats,
			},
			ConnectionInformation: &sdp.ConnectionInformation{
				NetworkType: "IN",
				AddressType: "IP4",
				Address:     &sdp.Address{Address: "0.0.0.0"},
			},
		}

		var attrs []sdp.Attribute

		// Copy codec attributes from Wowza
		for _, attr := range wowzaMD.Attributes {
			switch attr.Key {
			case "rtpmap", "fmtp", "rtcp-fb", "ssrc", "msid", "cliprect", "framesize", "control":
				attrs = append(attrs, attr)
			}
		}

		// Wowza's ICE/DTLS credentials for direct client-Wowza connection
		attrs = append(attrs,
			sdp.Attribute{Key: "ice-ufrag", Value: wowzaCreds.IceUfrag},
			sdp.Attribute{Key: "ice-pwd", Value: wowzaCreds.IcePwd},
			sdp.Attribute{Key: "fingerprint", Value: wowzaCreds.Fingerprint},
			// DTLS role: passive means Wowza waits for client to initiate DTLS handshake
			sdp.Attribute{Key: "setup", Value: "passive"},
			// CRITICAL: Must use client's mid values, not Wowza's (video/audio vs 0/1)
			sdp.Attribute{Key: "mid", Value: clientMediaInfo.Mid},
			sdp.Attribute{Key: "sendonly", Value: ""},
			sdp.Attribute{Key: "rtcp-mux", Value: ""},
		)

		// Add ICE candidates for this media section
		for _, c := range wowzaCandidates {
			if c.SDPMLineIndex != nil && int(*c.SDPMLineIndex) == i {
				cleaned := cleanWowzaCandidate(c.Candidate)
				cleaned = strings.TrimPrefix(cleaned, "candidate:")
				attrs = append(attrs, sdp.Attribute{Key: "candidate", Value: cleaned})
			}
		}

		md.Attributes = attrs
		answerDesc.MediaDescriptions = append(answerDesc.MediaDescriptions, md)
	}

	bytes, err := answerDesc.Marshal()
	if err != nil {
		return "", fmt.Errorf("marshal answer: %w", err)
	}

	return string(bytes), nil
}

// filterPrivateIPs removes private and IPv6 candidates for Wowza Cloud compatibility
func filterPrivateIPs(sdpStr string) string {
	lines := strings.Split(sdpStr, "\r\n")
	filtered := make([]string, 0, len(lines))

	for _, line := range lines {
		if strings.HasPrefix(line, "a=end-of-candidates") {
			continue
		}
		if strings.HasPrefix(line, "a=candidate:") {
			parts := strings.Fields(line)
			if len(parts) >= 5 {
				ip := net.ParseIP(parts[4])
				if ip != nil && (ip.To4() == nil || isPrivateIP(ip)) {
					continue
				}
			}
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\r\n")
}

func isPrivateIP(ip net.IP) bool {
	return ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast())
}

// addTrickleICE adds trickle ICE option after ice-ufrag
func addTrickleICE(sdpStr string) string {
	if strings.Contains(sdpStr, "a=ice-options:trickle") {
		return sdpStr
	}
	lines := strings.Split(sdpStr, "\r\n")
	result := make([]string, 0, len(lines)+1)
	added := false
	for _, line := range lines {
		result = append(result, line)
		if !added && strings.HasPrefix(line, "a=ice-ufrag:") {
			result = append(result, "a=ice-options:trickle")
			added = true
		}
	}
	return strings.Join(result, "\r\n")
}
