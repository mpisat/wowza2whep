/**
 * WHEP Player with Wowza SDP Munging
 *
 * This player handles the payload type (PT) mismatch between browsers and Wowza.
 * Wowza uses PT 97 for video (both H264 and VP8) and PT 96 for OPUS audio.
 *
 * The URL format determines which codec to expect:
 *   /whep/h264/{app}/{stream} - expect H264 at PT 97
 *   /whep/vp8/{app}/{stream}  - expect VP8 at PT 97
 *
 * The mungeOfferForWowzaPTs function rewrites the browser's SDP offer to include
 * Wowza's expected payload types BEFORE setLocalDescription() is called.
 *
 * Firefox limitation: Only H264 video works. VP8 and audio do not work due to
 * Firefox ignoring PT reassignment in munged SDPs. This is a Firefox WebRTC
 * implementation limitation combined with Wowza's fixed PT scheme.
 */

let pc = null;
let resourceUrl = null;
let audioContext = null;
let analyser = null;
let animationId = null;

const video = document.getElementById('video');
const videoWrapper = document.getElementById('videoWrapper');
const statusPanel = document.getElementById('statusPanel');
const statusText = document.getElementById('statusText');
const signalOverlay = document.getElementById('signalOverlay');
const signalDot = document.getElementById('signalDot');
const signalText = document.getElementById('signalText');
const logEl = document.getElementById('log');
const playBtn = document.getElementById('play');
const stopBtn = document.getElementById('stop');
const urlInput = document.getElementById('whepUrl');
const waveformCanvas = document.getElementById('waveform');
const statCodec = document.getElementById('statCodec');
const statRes = document.getElementById('statRes');

function log(msg, type = 'info') {
    const entry = document.createElement('div');
    entry.className = `log-entry ${type}`;
    const time = new Date().toLocaleTimeString('en-US', { hour12: false });
    entry.innerHTML = `<span class="log-time">${time}</span><span class="log-msg">${msg}</span>`;
    logEl.appendChild(entry);
    logEl.scrollTop = logEl.scrollHeight;
}

function setStatus(msg, state = '') {
    statusText.textContent = msg;
    statusPanel.className = 'status-panel ' + state;

    if (state === 'connected') {
        signalDot.classList.add('live');
        signalText.textContent = 'LIVE';
        signalOverlay.classList.add('active');
    } else if (state === 'connecting') {
        signalDot.classList.remove('live');
        signalText.textContent = 'CONNECTING';
        signalOverlay.classList.add('active');
    } else {
        signalDot.classList.remove('live');
        signalText.textContent = 'STANDBY';
        signalOverlay.classList.remove('active');
    }
}

function initAudioVisualization(stream) {
    try {
        audioContext = new (window.AudioContext || window.webkitAudioContext)();
        analyser = audioContext.createAnalyser();
        analyser.fftSize = 256;
        const source = audioContext.createMediaStreamSource(stream);
        source.connect(analyser);
        drawWaveform();
    } catch (e) {
        console.warn('Audio visualization not available:', e);
    }
}

function drawWaveform() {
    if (!analyser) return;

    const canvas = waveformCanvas;
    const ctx = canvas.getContext('2d');
    const rect = canvas.getBoundingClientRect();
    canvas.width = rect.width * window.devicePixelRatio;
    canvas.height = rect.height * window.devicePixelRatio;
    ctx.scale(window.devicePixelRatio, window.devicePixelRatio);

    const bufferLength = analyser.frequencyBinCount;
    const dataArray = new Uint8Array(bufferLength);

    function draw() {
        animationId = requestAnimationFrame(draw);
        analyser.getByteFrequencyData(dataArray);

        ctx.fillStyle = '#12141a';
        ctx.fillRect(0, 0, rect.width, rect.height);

        const barWidth = rect.width / bufferLength * 2.5;
        let x = 0;

        for (let i = 0; i < bufferLength; i++) {
            const barHeight = (dataArray[i] / 255) * (rect.height - 12);
            const gradient = ctx.createLinearGradient(0, rect.height, 0, 0);
            gradient.addColorStop(0, '#00d4ff');
            gradient.addColorStop(1, '#00ff88');
            ctx.fillStyle = gradient;
            ctx.fillRect(x, rect.height - barHeight - 6, barWidth - 1, barHeight);
            x += barWidth;
        }
    }
    draw();
}

function stopAudioVisualization() {
    if (animationId) {
        cancelAnimationFrame(animationId);
        animationId = null;
    }
    if (audioContext) {
        audioContext.close();
        audioContext = null;
        analyser = null;
    }
    const ctx = waveformCanvas.getContext('2d');
    ctx.fillStyle = '#12141a';
    ctx.fillRect(0, 0, waveformCanvas.width, waveformCanvas.height);
}

function updateStats() {
    if (!video.srcObject) {
        statCodec.textContent = '--';
        statRes.textContent = '--';
        return;
    }

    const videoTrack = video.srcObject.getVideoTracks()[0];
    if (videoTrack) {
        const settings = videoTrack.getSettings();
        if (settings.width && settings.height) {
            statRes.textContent = `${settings.width}x${settings.height}`;
        }
    }

    if (pc) {
        pc.getStats().then(stats => {
            stats.forEach(report => {
                if (report.type === 'inbound-rtp' && report.kind === 'video') {
                    const codecId = report.codecId;
                    if (codecId) {
                        const codecReport = stats.get(codecId);
                        if (codecReport && codecReport.mimeType) {
                            statCodec.textContent = codecReport.mimeType.split('/')[1].toUpperCase();
                        }
                    }
                }
            });
        });
    }
}

/**
 * Extract video codec from WHEP URL path.
 * URL format: /whep/{codec}/{app}/{stream} or /whep/cloud/{codec}/{host}/{app}/{stream}
 * Returns 'h264' or 'vp8', defaults to 'h264' if not found.
 */
function extractCodecFromUrl(url) {
    try {
        const urlObj = new URL(url);
        const path = urlObj.pathname;

        // Match /whep/cloud/{codec}/... or /whep/{codec}/...
        const cloudMatch = path.match(/\/whep\/cloud\/(h264|vp8)\//i);
        if (cloudMatch) return cloudMatch[1].toLowerCase();

        const directMatch = path.match(/\/whep\/(h264|vp8)\//i);
        if (directMatch) return directMatch[1].toLowerCase();

        log('No codec in URL, defaulting to h264', 'warn');
        return 'h264';
    } catch (e) {
        return 'h264';
    }
}

/**
 * Munge SDP to match Wowza's fixed payload type scheme.
 *
 * Wowza uses: PT 97 for video (H264 or VP8), PT 96 for OPUS audio.
 * This function strips all existing codecs and adds only PT 97 video + PT 96 audio.
 *
 * CRITICAL: Must happen BEFORE setLocalDescription() - browser's internal PT
 * expectations are locked at that point.
 */
function mungeOfferForWowzaPTs(sdp, codec) {
    const lines = sdp.split('\r\n');
    const result = [];

    // Find all video PTs to remove (clock rate 90000 = video)
    const allVideoPts = new Set();
    for (const line of lines) {
        const match = line.match(/^a=rtpmap:(\d+)\s+\S+\/90000/);
        if (match) {
            allVideoPts.add(match[1]);
        }
        const fmtpMatch = line.match(/^a=fmtp:(\d+)\s+apt=\d+/);
        if (fmtpMatch) {
            allVideoPts.add(fmtpMatch[1]);
        }
    }

    // Find all audio PTs to remove (clock rate 48000 = OPUS)
    const allAudioPts = new Set();
    for (const line of lines) {
        const match = line.match(/^a=rtpmap:(\d+)\s+\S+\/48000/);
        if (match) {
            allAudioPts.add(match[1]);
        }
    }

    let inVideoSection = false;
    let inAudioSection = false;
    let addedVideoCodec = false;
    let addedAudioCodec = false;

    for (const line of lines) {
        if (line.startsWith('m=video')) {
            inVideoSection = true;
            inAudioSection = false;
            // Only PT 97 for video
            const parts = line.split(' ');
            const header = parts.slice(0, 3);
            result.push([...header, '97'].join(' '));
            continue;
        } else if (line.startsWith('m=audio')) {
            inVideoSection = false;
            inAudioSection = true;
            // Only PT 96 for audio
            const parts = line.split(' ');
            const header = parts.slice(0, 3);
            result.push([...header, '96'].join(' '));
            continue;
        } else if (line.startsWith('m=')) {
            inVideoSection = false;
            inAudioSection = false;
        }

        // Remove all video codec definitions
        if (inVideoSection) {
            const ptMatch = line.match(/^a=(rtpmap|rtcp-fb|fmtp):(\d+)/);
            if (ptMatch && allVideoPts.has(ptMatch[2])) {
                continue;
            }
        }

        // Remove all audio codec definitions
        if (inAudioSection) {
            const ptMatch = line.match(/^a=(rtpmap|rtcp-fb|fmtp):(\d+)/);
            if (ptMatch && allAudioPts.has(ptMatch[2])) {
                continue;
            }
        }

        result.push(line);

        // Add video codec at PT 97
        if (inVideoSection && !addedVideoCodec && (line.startsWith('a=recvonly') || line.startsWith('a=sendrecv') || line.startsWith('a=sendonly'))) {
            if (codec === 'vp8') {
                result.push('a=rtpmap:97 VP8/90000');
                result.push('a=fmtp:97 max-fs=12288;max-fr=60');
            } else {
                result.push('a=rtpmap:97 H264/90000');
                result.push('a=fmtp:97 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f');
            }
            result.push('a=rtcp-fb:97 goog-remb');
            result.push('a=rtcp-fb:97 transport-cc');
            result.push('a=rtcp-fb:97 ccm fir');
            result.push('a=rtcp-fb:97 nack');
            result.push('a=rtcp-fb:97 nack pli');
            addedVideoCodec = true;
        }

        // Add OPUS at PT 96
        if (inAudioSection && !addedAudioCodec && (line.startsWith('a=recvonly') || line.startsWith('a=sendrecv') || line.startsWith('a=sendonly'))) {
            result.push('a=rtpmap:96 opus/48000/2');
            result.push('a=fmtp:96 maxplaybackrate=48000;stereo=1;useinbandfec=1');
            result.push('a=rtcp-fb:96 transport-cc');
            addedAudioCodec = true;
        }
    }

    return result.join('\r\n');
}

async function play() {
    const whepUrl = urlInput.value.trim();
    if (!whepUrl) {
        log('Please enter a WHEP URL', 'error');
        return;
    }

    const codec = extractCodecFromUrl(whepUrl);
    log(`Using codec: ${codec.toUpperCase()} at PT 97`);

    try {
        playBtn.disabled = true;
        setStatus('Connecting...', 'connecting');
        log('Initializing WebRTC connection');

        pc = new RTCPeerConnection({
            iceServers: [{ urls: 'stun:stun.l.google.com:19302' }]
        });

        pc.addTransceiver('video', { direction: 'recvonly' });
        pc.addTransceiver('audio', { direction: 'recvonly' });

        pc.ontrack = (e) => {
            log(`Track received: ${e.track.kind}`, 'success');
            if (video.srcObject !== e.streams[0]) {
                video.srcObject = e.streams[0];
                videoWrapper.classList.add('has-video');
                initAudioVisualization(e.streams[0]);
                setTimeout(updateStats, 1000);
            }
        };

        pc.oniceconnectionstatechange = () => {
            const state = pc.iceConnectionState;
            log(`ICE state: ${state}`, state === 'connected' ? 'success' : 'info');
            if (state === 'connected') {
                setStatus('Connected', 'connected');
                setTimeout(updateStats, 500);
            } else if (state === 'failed') {
                setStatus('Connection failed', 'error');
                log('ICE connection failed', 'error');
            } else if (state === 'disconnected') {
                setStatus('Disconnected', 'error');
            }
        };

        const offer = await pc.createOffer();
        const mungedOffer = mungeOfferForWowzaPTs(offer.sdp, codec);
        await pc.setLocalDescription({ type: 'offer', sdp: mungedOffer });

        // Wait for ICE gathering to complete (or timeout after 2s)
        await new Promise((resolve) => {
            if (pc.iceGatheringState === 'complete') resolve();
            else {
                const timeout = setTimeout(resolve, 2000);
                pc.onicegatheringstatechange = () => {
                    if (pc.iceGatheringState === 'complete') { clearTimeout(timeout); resolve(); }
                };
            }
        });

        log(`POST ${whepUrl}`);
        const res = await fetch(whepUrl, {
            method: 'POST',
            headers: { 'Content-Type': 'application/sdp' },
            body: pc.localDescription.sdp
        });

        if (!res.ok) throw new Error(`WHEP error ${res.status}: ${await res.text()}`);

        const answerSdp = await res.text();
        resourceUrl = new URL(res.headers.get('Location'), whepUrl).href;
        log(`Session created`, 'success');

        await pc.setRemoteDescription({ type: 'answer', sdp: answerSdp });
        setStatus('Negotiating...', 'connecting');
        stopBtn.disabled = false;

    } catch (err) {
        log(`Error: ${err.message}`, 'error');
        setStatus('Error', 'error');
        stop();
    }
}

async function stop() {
    if (resourceUrl) {
        try { await fetch(resourceUrl, { method: 'DELETE' }); } catch {}
        resourceUrl = null;
    }
    if (pc) { pc.close(); pc = null; }
    video.srcObject = null;
    videoWrapper.classList.remove('has-video');
    stopAudioVisualization();
    statCodec.textContent = '--';
    statRes.textContent = '--';
    playBtn.disabled = false;
    stopBtn.disabled = true;
    setStatus('Ready');
    log('Session ended');
}

// Initialize
playBtn.onclick = play;
stopBtn.onclick = stop;

const params = new URLSearchParams(window.location.search);
if (params.get('url')) urlInput.value = params.get('url');

log('Player ready');
