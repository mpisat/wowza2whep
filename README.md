# wowza2whep

A **signaling-only** WHEP gateway for Wowza Streaming Engine. Media flows directly between browser and Wowza - the gateway handles protocol translation only.

## Benefits

- **Faster time-to-first-frame** - Single HTTP request/response replaces Wowza's multi-step WebSocket handshake
- **Minimal client-side code** - Server handles SDP credential swapping, mid translation, and ICE candidate cleanup. Browser only needs ~100 lines of PT munging
- **No Wowza SDK dependency** - Standard WHEP protocol works with any WHEP-compatible player
- **Zero media overhead** - Signaling-only design means no added latency or bandwidth cost

## Architecture

```
Browser ──HTTP POST──> Gateway ──WebSocket──> Wowza
   │                                            │
   │         [SDP translation only]             │
   │         [No media relay]                   │
   │         [No RTP packet processing]         │
   │                                            │
   └────────── Direct WebRTC Media ─────────────┘
```

**Server handles:**
- Protocol translation (HTTP ↔ WebSocket + JSON)
- Signaling flow inversion (Wowza sends offer, WHEP expects client offer)
- ICE/DTLS credential swapping for direct client↔Wowza connection
- Mid value mapping (`0`,`1` ↔ `video`,`audio`)
- ICE candidate cleanup (removes `generation X`, adds `tcptype passive`)
- Private IP filtering

**Browser handles:**
- Payload type munging only - see `static/whep-munge-sdp.js`

**Not in path:**
- No media relay
- No RTP rewriting
- No WebRTC termination

## Why This Exists

Wowza Streaming Engine does not implement WHEP (RFC 9725). Without this gateway, you'd need Wowza's JavaScript SDK which:
- Adds significant weight to your bundle
- Requires WebSocket connection management
- Handles complex multi-step signaling

This gateway moves that complexity server-side:

| Aspect | WHEP | Wowza Native | This Gateway |
|--------|------|--------------|--------------|
| **Protocol** | HTTP | WebSocket + JSON | HTTP (server bridges) |
| **Signaling** | Client offers | Server offers | Bridged |
| **Round trips** | 1 | 3+ | 1 |
| **Client code** | Standard WHEP | Wowza SDK | ~100 lines PT munge |

## The Payload Type Problem

Wowza uses fixed payload types regardless of codec:
- **PT 97** = Video (H264 or VP8)
- **PT 96** = Audio (OPUS)

Browsers typically assign different PTs. Without intervention, packets arrive but don't decode.

**Solution**: Browser-side SDP munging **before** `setLocalDescription()` to match Wowza's PT scheme. The test player (`static/test.html`) demonstrates this approach.

## Requirements

- Go 1.23 or later

## Installation

```bash
go install github.com/mpisat/wowza2whep@latest
```

Or build from source:

```bash
git clone https://github.com/mpisat/wowza2whep.git
cd wowza2whep
go build -o wowza2whep .
```

## Usage

### Static Mode (Single Wowza Instance)

```bash
./wowza2whep -websocket wss://wowza.example.com/webrtc-session.json
```

Endpoints:
- `POST /whep/h264/{app}/{stream}` - H264 streams
- `POST /whep/vp8/{app}/{stream}` - VP8 streams

### Dynamic Mode (Multiple Wowza Hosts)

```bash
./wowza2whep
```

Endpoints:
- `POST /whep/cloud/h264/{host}/{app}/{stream}`
- `POST /whep/cloud/vp8/{host}/{app}/{stream}`

Where `{host}` is:
- FQDN: `wowza.example.com` → `wss://{host}/webrtc-session.json` (on-prem or self-hosted)
- Wowza Cloud ID: `c334d88b` → `wss://{id}.entrypoint.cloud.wowza.com/webrtc-session.json`

Note: The `/cloud/` path segment is historical naming - it works with any Wowza instance.

### Configuration

| Flag | Environment | Default | Description |
|------|-------------|---------|-------------|
| `-listen` | `LISTEN_ADDR` | `:8080` | HTTP listen address |
| `-websocket` | `WOWZA_WEBSOCKET_URL` | - | Static mode Wowza URL |
| `-allowed-hosts` | `ALLOWED_HOSTS` | `*` | Allowed hosts (comma-separated) |
| `-insecure-tls` | `INSECURE_TLS` | `false` | Skip TLS verification |
| `-verbose` | `VERBOSE` | `false` | Debug logging |

### Test Player

Built-in player at `http://localhost:8080/static/test.html`

## Browser Compatibility

| Browser | H264 Video | VP8 Video | Audio | Notes |
|---------|------------|-----------|-------|-------|
| Chrome | Yes | Yes | Yes | Full support |
| Safari | Yes | Yes | Yes | Full support |
| Firefox | Yes | No | No | Video-only, H264 only |

### Firefox Limitation

Firefox only supports H264 video - no VP8, no audio. This is due to Wowza's hardcoded payload types conflicting with Firefox's native assignments:

| Media | Wowza PT | Firefox Native PT | Compatible? |
|-------|----------|-------------------|-------------|
| H264 | 97 | 97 | Yes (coincidence) |
| VP8 | 97 | 120 | No |
| OPUS | 96 | 109 | No |

**Why H264 video works**: Firefox and Wowza both happen to use PT 97 for H264. This is a coincidental match - no PT remapping occurs.

**Why VP8/audio fail**: Wowza sends VP8 at PT 97 and OPUS at PT 96. Firefox expects VP8 at PT 120 and OPUS at PT 109. SDP munging cannot fix this because Firefox ignores PT reassignment in munged SDPs (unlike Chrome/Safari which honor it).

**Technical background**: Per [webrtcHacks](https://webrtchacks.com/not-a-guide-to-sdp-munging/), Firefox only honors codec removal/reordering in munged SDPs, not PT reassignment. The [MDN docs](https://developer.mozilla.org/en-US/docs/Web/API/RTCRtpTransceiver/setCodecPreferences) recommend `setCodecPreferences()` before `createOffer()`, but this only controls codec order, not PT numbers.

**Recommendation**: For Firefox users, use H264 encoding and accept video-only playback, or implement a media relay to rewrite PT numbers (which defeats the signaling-only architecture).

## API

### POST /whep/{codec}/{app}/{stream}

Create WHEP session. Codec: `h264` or `vp8`.

**Request**: `Content-Type: application/sdp` with SDP offer body

**Response**: `201 Created` with SDP answer, `Location` header for session URL

### DELETE /whep/{codec}/{app}/{stream}/{session-id}

Close session (RFC compliance - WebSocket already closed after SDP exchange).

### GET /health

Health check.

### GET /stats

Session statistics.

## License

MIT
