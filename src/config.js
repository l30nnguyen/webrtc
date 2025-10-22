import dotenv from 'dotenv';
dotenv.config();

export const CONFIG = {
  signaling: {
    port: parseInt(process.env.SIGNALING_PORT) || 1988,
    host: process.env.SIGNALING_HOST || "0.0.0.0"
  },
  udp: {
    port: parseInt(process.env.UDP_PORT) || 8554,
    host: process.env.UDP_HOST || "0.0.0.0"
  },
  rtp: {
    payloadType: parseInt(process.env.RTP_PAYLOAD_TYPE) || 96,
    clockRate: parseInt(process.env.RTP_CLOCK_RATE) || 90000,
    fps: parseInt(process.env.RTP_FPS) || 30,
    mtu: parseInt(process.env.RTP_MTU) || 1200
  },
  ice: {
    stun: process.env.STUN_SERVER || "stun:stun.l.google.com:19302",
    turn: process.env.TURN_SERVER ? {
      urls: process.env.TURN_SERVER,
      username: process.env.TURN_USERNAME,
      credential: process.env.TURN_PASSWORD
    } : null
  },
  logging: {
    level: process.env.LOG_LEVEL || 'info',
    debug: process.env.DEBUG === 'true'
  }
};
