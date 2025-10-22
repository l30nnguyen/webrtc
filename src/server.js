import {CONFIG} from "./config.js";
import express from "express";
import cors from "cors";
import dgram from "dgram";
import { RTCPeerConnection, RTCRtpCodecParameters, MediaStreamTrack } from "werift";

// ===== Global State =====
const connections = new Map();
const spsppscache = { sps: null, pps: null };
let udpServer = null;
let stats = {
  totalConnections: 0,
  activeConnections: 0,
  packetsReceived: 0,
  bytesReceived: 0,
  startTime: Date.now()
};

// ===== H.264 NALU Parser =====
class H264NALUParser {
  constructor() {
    this.buffer = Buffer.alloc(0);
    this.startCodeLength = 0;
  }

  feed(data) {
    this.buffer = Buffer.concat([this.buffer, data]);
    return this.extract();
  }

  extract() {
    const nalus = [];
    let offset = 0;

    while (offset < this.buffer.length - 4) {
      // Look for start codes: 0x00000001 or 0x000001
      const startCode4 = this.buffer.readUInt32BE(offset);
      const startCode3 = (this.buffer[offset] === 0x00 && 
                         this.buffer[offset + 1] === 0x00 && 
                         this.buffer[offset + 2] === 0x01);

      if (startCode4 === 0x00000001) {
        this.startCodeLength = 4;
      } else if (startCode3) {
        this.startCodeLength = 3;
      } else {
        offset++;
        continue;
      }

      // Find next start code
      let nextOffset = offset + this.startCodeLength;
      let found = false;

      while (nextOffset < this.buffer.length - 4) {
        const next4 = this.buffer.readUInt32BE(nextOffset);
        const next3 = (this.buffer[nextOffset] === 0x00 && 
                       this.buffer[nextOffset + 1] === 0x00 && 
                       this.buffer[nextOffset + 2] === 0x01);

        if (next4 === 0x00000001 || next3) {
          found = true;
          break;
        }
        nextOffset++;
      }

      if (found) {
        const naluWithStartCode = this.buffer.subarray(offset, nextOffset);
        const nalu = this.buffer.subarray(offset + this.startCodeLength, nextOffset);
        const naluType = nalu[0] & 0x1f;

        nalus.push({ nalu: naluWithStartCode, payload: nalu, type: naluType });
        offset = nextOffset;
      } else {
        break;
      }
    }

    // Keep remaining data in buffer
    this.buffer = this.buffer.subarray(offset);
    return nalus;
  }
}

// ===== H.264 RTP Packetizer =====
class H264RTPPacketizer {
  constructor({ payloadType, ssrc, clockRate, mtu = 1200 }) {
    this.payloadType = payloadType;
    this.ssrc = ssrc || Math.floor(Math.random() * 0xFFFFFFFF);
    this.clockRate = clockRate;
    this.mtu = mtu;
    this.seq = 0;
  }

  packetize(naluPayload, marker, timestamp) {
    const maxPayloadSize = this.mtu - 12;
    const packets = [];

    if (naluPayload.length <= maxPayloadSize) {
      // Single NALU packet
      packets.push(this.createRTPPacket(naluPayload, marker, timestamp));
    } else {
      // Fragmentation (FU-A)
      const naluHeader = naluPayload[0];
      const nri = (naluHeader >> 5) & 0x03;
      const naluType = naluHeader & 0x1f;
      const fuIndicator = (nri << 5) | 28; // Type 28 = FU-A

      const naluData = naluPayload.subarray(1);
      const maxFragmentSize = maxPayloadSize - 2;
      let offset = 0;

      while (offset < naluData.length) {
        const isStart = offset === 0;
        const isEnd = offset + maxFragmentSize >= naluData.length;
        const fragmentEnd = isEnd ? naluData.length : offset + maxFragmentSize;
        const isMarker = isEnd && marker;

        const fuHeader = (isStart ? 0x80 : 0x00) | (isEnd ? 0x40 : 0x00) | naluType;
        const fragment = Buffer.alloc(fragmentEnd - offset + 2);
        fragment[0] = fuIndicator;
        fragment[1] = fuHeader;
        naluData.copy(fragment, 2, offset, fragmentEnd);

        packets.push(this.createRTPPacket(fragment, isMarker, timestamp));
        offset = fragmentEnd;
      }
    }

    return packets;
  }

  createRTPPacket(payload, marker, timestamp) {
    const packet = Buffer.alloc(12 + payload.length);
    
    packet[0] = 0x80; // V=2, P=0, X=0, CC=0
    packet[1] = this.payloadType | (marker ? 0x80 : 0x00);
    packet.writeUInt16BE(this.seq, 2);
    this.seq = (this.seq + 1) % 65536;
    packet.writeUInt32BE(timestamp >>> 0, 4);
    packet.writeUInt32BE(this.ssrc >>> 0, 8);
    payload.copy(packet, 12);

    return packet;
  }
}

// ===== UDP H.264 Stream Receiver =====
function startUDPReceiver() {
  udpServer = dgram.createSocket('udp4');
  const parser = new H264NALUParser();

  udpServer.on('message', (data) => {
    stats.packetsReceived++;
    stats.bytesReceived += data.length;

    const nalus = parser.feed(data);
    
    nalus.forEach(({ payload, type }) => {
      // Cache SPS/PPS
      if (type === 7) {
        spsppscache.sps = Buffer.from(payload);
        // console.log(`📦 Cached SPS: ${payload.length} bytes`);
      } else if (type === 8) {
        spsppscache.pps = Buffer.from(payload);
        // console.log(`📦 Cached PPS: ${payload.length} bytes`);
      }

      // Broadcast to all connections
      broadcastNALU(payload, type);
    });
  });

  udpServer.on('error', (err) => {
    console.error('❌ UDP Server Error:', err);
  });

  udpServer.bind(CONFIG.udp.port, CONFIG.udp.host, () => {
    console.log(`✅ UDP receiver listening on ${CONFIG.udp.host}:${CONFIG.udp.port}`);
  });
}

// ===== Broadcast NALU to All Connections =====
function broadcastNALU(naluPayload, naluType) {
  const isFrame = naluType === 1 || naluType === 5; // P-frame or I-frame
  const isIDR = naluType === 5;

  connections.forEach((conn) => {
    if (!conn.active || conn.senders.length === 0) return;

    try {
      // Send SPS/PPS before first I-frame
      if (isIDR && !conn.sentSPSPPS && spsppscache.sps && spsppscache.pps) {
        
        const spsPackets = conn.packetizer.packetize(spsppscache.sps, false, conn.timestamp);
        spsPackets.forEach(pkt => conn.senders.forEach(s => s.sendRtp(pkt)));

        const ppsPackets = conn.packetizer.packetize(spsppscache.pps, false, conn.timestamp);
        ppsPackets.forEach(pkt => conn.senders.forEach(s => s.sendRtp(pkt)));

        conn.sentSPSPPS = true;
      }

      // console.log(`🔑 [${conn.id}] Sending Frame ${naluType}`);
      // Send the frame
      const packets = conn.packetizer.packetize(naluPayload, isFrame, conn.timestamp);
      packets.forEach(packet => {
        conn.senders.forEach(sender => {
          try {
            sender.sendRtp(packet);
          } catch (err) {
            console.error(`[${conn.id}] RTP send error:`, err.message);
          }
        });
      });

      // Increment timestamp after complete frame
      if (isFrame) {
        conn.timestamp = (conn.timestamp + (CONFIG.rtp.clockRate / CONFIG.rtp.fps)) >>> 0;
        conn.frameCount++;

        if (conn.frameCount % 150 === 0) {
          console.log(`📊 [${conn.id}] Sent ${conn.frameCount} frames`);
        }
      }
    } catch (err) {
      console.error(`[${conn.id}] Error broadcasting:`, err);
    }
  });
}

// ===== WebRTC Peer Connection Handler =====
async function handleOffer(offerSdp) {
  const iceServers = [{ urls: CONFIG.ice.stun }];
  if (CONFIG.ice.turn) {
    iceServers.push(CONFIG.ice.turn);
  }

  console.log(iceServers)
  const pc = new RTCPeerConnection({
    bundlePolicy: "max-bundle",
    iceServers
  });

  const connId = `conn_${Date.now()}_${Math.random().toString(36).substr(2, 6)}`;

  // Connection state handlers
  pc.oniceconnectionstatechange = () => {
    console.log(`[${connId}] ICE: ${pc.iceConnectionState}`);
    if (['failed', 'closed', 'disconnected'].includes(pc.iceConnectionState)) {
      setTimeout(() => cleanupConnection(connId), 3000);
    }
  };

  pc.onconnectionstatechange = () => {
    console.log(`[${connId}] Connection: ${pc.connectionState}`);
    if (['failed', 'closed'].includes(pc.connectionState)) {
      cleanupConnection(connId);
    }
  };

  // Create video track and transceiver
  const track = new MediaStreamTrack({ kind: "video" });
  const transceiver = pc.addTransceiver(track, { direction: "sendonly" });

  // Configure H.264 codec
  const h264Codec = new RTCRtpCodecParameters({
    mimeType: "video/H264",
    clockRate: CONFIG.rtp.clockRate,
    payloadType: CONFIG.rtp.payloadType,
    rtcpFeedback: [
      { type: "nack" },
      { type: "nack", parameter: "pli" }
    ]
  });

  transceiver.codecs = [h264Codec];

  // Process SDP
  await pc.setRemoteDescription({ type: "offer", sdp: offerSdp });
  const answer = await pc.createAnswer();

  // Modify SDP to include SPS/PPS if available
  let modifiedSdp = answer.sdp;
  if (spsppscache.sps && spsppscache.pps) {
    const spsBase64 = spsppscache.sps.toString('base64');
    const ppsBase64 = spsppscache.pps.toString('base64');
    const rtpmapPattern = new RegExp(`a=rtpmap:${CONFIG.rtp.payloadType} (.*)/${CONFIG.rtp.clockRate}`, 'i');
    
    if (rtpmapPattern.test(modifiedSdp)) {
      modifiedSdp = modifiedSdp.replace(
        rtpmapPattern,
        `a=rtpmap:${CONFIG.rtp.payloadType} H264/${CONFIG.rtp.clockRate}\r\na=fmtp:${CONFIG.rtp.payloadType} level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f;sprop-parameter-sets=${spsBase64},${ppsBase64}`
      );
      console.log(`✅ [${connId}] Added SPS/PPS to SDP`);
    }
  }
  console.log(modifiedSdp)
  await pc.setLocalDescription({ type: "answer", sdp: modifiedSdp });

  // Create connection state
  const packetizer = new H264RTPPacketizer({
    payloadType: CONFIG.rtp.payloadType,
    clockRate: CONFIG.rtp.clockRate,
    ssrc: getSsrcFromSdp(pc.localDescription.sdp),
    mtu: CONFIG.rtp.mtu
  });

  connections.set(connId, {
    id: connId,
    pc,
    senders: pc.getSenders(),
    packetizer,
    track,
    timestamp: Math.floor(Math.random() * 0xFFFFFFFF),
    frameCount: 0,
    sentSPSPPS: false,
    active: true,
    createdAt: Date.now()
  });

  stats.totalConnections++;
  stats.activeConnections = connections.size;

  console.log(`✅ [${connId}] Connection established. Total: ${connections.size}`);

  return { sdp: pc.localDescription.sdp, connectionId: connId };
}


function getSsrcFromSdp(sdp){
    const lines = sdp.split('\n');
    let vidSsrc = 0;
    for (const line of lines) {
        if (line.startsWith('a=ssrc:')) {
            vidSsrc = Number(line.split('a=ssrc:')[1].split(' ')[0])
            break;
        }
    }
    return vidSsrc
}

// ===== Cleanup Connection =====
function cleanupConnection(connId) {
  const conn = connections.get(connId);
  if (!conn) return;

  conn.active = false;
  try {
    conn.track?.stop();
    conn.pc?.close();
  } catch (err) {
    console.error(`[${connId}] Cleanup error:`, err.message);
  }

  connections.delete(connId);
  stats.activeConnections = connections.size;
  console.log(`🗑️ [${connId}] Cleaned up. Remaining: ${connections.size}`);
}

// ===== Express Server =====
const app = express();
app.use(cors());
app.use(express.json());
app.use(express.text({ type: "*/*" }));

app.post("/offer", async (req, res) => {
  try {
    const offerSdp = req.body;
    const { sdp, connectionId } = await handleOffer(offerSdp);
    
    res.json({
      code: 0,
      type: "answer",
      sdp,
      connectionId
    });
  } catch (error) {
    console.error("❌ Error handling offer:", error);
    res.status(500).json({ code: -1, error: error.message });
  }
});

app.get("/stats", (req, res) => {
  const uptime = Math.floor((Date.now() - stats.startTime) / 1000);
  res.json({
    ...stats,
    uptime,
    hasSPS: !!spsppscache.sps,
    hasPPS: !!spsppscache.pps,
    connectionDetails: Array.from(connections.values()).map(c => ({
      id: c.id,
      frameCount: c.frameCount,
      sentSPSPPS: c.sentSPSPPS,
      iceState: c.pc.iceConnectionState,
      connectionState: c.pc.connectionState
    }))
  });
});

app.get("/health", (req, res) => {
  res.json({ status: "ok", connections: connections.size });
});

app.listen(CONFIG.signaling.port, CONFIG.signaling.host, () => {
  console.log(`✅ Signaling server: http://${CONFIG.signaling.host}:${CONFIG.signaling.port}`);
});

// ===== Start Services =====
startUDPReceiver();

// ===== Graceful Shutdown =====
process.on('SIGINT', () => {
  console.log('\n🛑 Shutting down...');
  connections.forEach((_, id) => cleanupConnection(id));
  udpServer?.close();
  process.exit(0);
});

console.log('🚀 WebRTC H.264 Streaming Server Started');
