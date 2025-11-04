#!/usr/bin/env python3
"""
Complete RTSP Latency Audit Tool
Identifies where latency accumulates in the entire pipeline
"""

import time
import argparse
import av
from collections import deque
from datetime import datetime

class RTSPLatencyAuditor:
    def __init__(self, rtsp_url):
        self.rtsp_url = rtsp_url
        self.timestamps = {
            'connection_start': None,
            'connection_complete': None,
            'first_packet_received': None,
            'first_frame_decoded': None,
            'first_iframe_decoded': None
        }
        self.packet_times = deque(maxlen=100)
        self.decode_times = deque(maxlen=100)
        self.frame_intervals = deque(maxlen=100)
        self.last_frame_time = None
        
    def get_options(self):
        return {
            # --------------------------------------------------------------------------
            # Mobile APP
            # --------------------------------------------------------------------------
            
            # Network/RTSP Transport (Format Options)
            'rtsp_transport': 'tcp',      # Equivalent to fijkOption.setFormatOption("rtsp_transport", "tcp")
            'rtsp_flags': 'prefer_tcp',   # Equivalent to fijkOption.setFormatOption("rtsp_flags", "prefer_tcp")
            
            # Probe/Analysis Reduction (CRITICAL for TTFF) 💥
            'analyzeduration': '100',     # Equivalent to fijkOption.setFormatOption("analyzeduration", 100). Aggressive (100 us).
            'probesize': '1024',          # Equivalent to fijkOption.setFormatOption("probesize", 1024) (Android value) - Very aggressive (1 KB).
            
            # Low-Latency Flags
            'fflags': 'nobuffer',         # Equivalent to fijkOption.setFormatOption("fflags", "nobuffer")
            'flags': 'low_delay',         # Equivalent to fijkOption.setPlayerOption("fast", 1) (often implies low_delay)
            
            # Stream Metadata Hints (Avoiding Probe)
            'vcodec': 'h264',             # Equivalent to fijkOption.setFormatOption("vcodec", "h264")
            
            # Other Format Options
            'timeout': '0',               # Equivalent to fijkOption.setFormatOption("timeout", 0). (NOTE: Set socket timeout in PyAV's wrapper instead)
            'http-detect-range-support': '0', # Equivalent to fijkOption.setFormatOption("http-detect-range-support", 0)
            'flush_packets': '1',         # Equivalent to fijkOption.setFormatOption("flush_packets", 1)
            'max_delay': '0',             # Equivalent to fijkOption.setFormatOption("max_delay", 0)
            
            # --------------------------------------------------------------------------
            # 🟡 BUFFER/PLAYER OPTIONS (Can be set as general options in PyAV)
            # --------------------------------------------------------------------------
            
            'infbuf': '1',                # Equivalent to fijkOption.setFormatOption("infbuf", 1) - Use infinite buffer.
            'min_frames': '2',            # Equivalent to fijkOption.setPlayerOption("min-frames", 2)
            'framedrop': '0',             # Equivalent to fijkOption.setPlayerOption("framedrop", 0)
            'max_cached_duration': '0',   # Equivalent to fijkOption.setPlayerOption("max_cached_duration", 0)
        }
        
    def audit_latency(self, duration=10, verbose=True):
        """
        Comprehensive latency audit
        
        Tracks:
        1. Network receive latency (packet arrival intervals)
        2. Decoder latency (packet → decoded frame)
        3. Frame timing (presentation timestamps vs actual)
        4. Jitter and buffering
        """
        
        print("="*70)
        print("🔍 RTSP Latency Audit")
        print("="*70)
        print(f"URL: {self.rtsp_url}")
        print(f"Duration: {duration}s")
        print(f"{'='*70}\n")
        
        # Phase 1: Connection
        print("📡 Phase 1: Connection & Handshake")
        print("-"*70)
        
        self.timestamps['connection_start'] = time.perf_counter()
        
        try:
            container = av.open(
                self.rtsp_url,
                options=self.get_options(),
                timeout=10.0
            )
            
            self.timestamps['connection_complete'] = time.perf_counter()
            conn_time = (self.timestamps['connection_complete'] - 
                        self.timestamps['connection_start']) * 1000
            
            print(f"✅ Connection established: {conn_time:.2f}ms")
            
            if conn_time > 1000:
                print(f"   ⚠️  WARNING: Connection took >{conn_time/1000:.1f}s")
                print(f"   → Check: DNS, TCP handshake, RTSP DESCRIBE/SETUP latency")
            
        except Exception as e:
            print(f"❌ Connection failed: {e}")
            return
        
        # Get video stream
        video_stream = next((s for s in container.streams if s.type == 'video'), None)
        if not video_stream:
            print("❌ No video stream found")
            return
        
        print(f"\n📺 Stream Info:")
        print(f"   Codec: {video_stream.codec_context.name}")
        print(f"   Resolution: {video_stream.width}x{video_stream.height}")
        print(f"   FPS: {video_stream.average_rate}")
        print(f"   Time Base: {video_stream.time_base}")
        
        # Phase 2: Packet & Frame Analysis
        print(f"\n📦 Phase 2: Packet & Frame Analysis")
        print("-"*70)
        
        start_time = time.perf_counter()
        frame_count = 0
        packet_count = 0
        iframe_count = 0
        
        last_packet_time = None
        last_pts = None
        
        # Latency accumulation tracking
        network_latencies = []
        decode_latencies = []
        pts_presentation_gaps = []
        
        try:
            for packet in container.demux(video_stream):
                if time.perf_counter() - start_time > duration:
                    break
                
                packet_count += 1
                packet_receive_time = time.perf_counter()
                
                # Track packet arrival intervals
                if last_packet_time:
                    interval = (packet_receive_time - last_packet_time) * 1000
                    self.packet_times.append(interval)
                
                if self.timestamps['first_packet_received'] is None:
                    self.timestamps['first_packet_received'] = packet_receive_time
                    first_packet_time = (packet_receive_time - 
                                       self.timestamps['connection_start']) * 1000
                    print(f"✅ First packet: {first_packet_time:.2f}ms from start")
                
                last_packet_time = packet_receive_time
                
                # Decode frames
                decode_start = time.perf_counter()
                
                for frame in packet.decode():
                    decode_end = time.perf_counter()
                    decode_time = (decode_end - decode_start) * 1000
                    decode_latencies.append(decode_time)
                    
                    frame_count += 1
                    frame_receive_time = decode_end
                    
                    # Frame type
                    is_keyframe = frame.key_frame
                    if is_keyframe:
                        iframe_count += 1
                    
                    # PTS analysis
                    pts = frame.pts
                    if pts is not None and last_pts is not None:
                        pts_diff = (pts - last_pts) * float(video_stream.time_base)
                        if self.last_frame_time:
                            actual_diff = frame_receive_time - self.last_frame_time
                            gap = abs(actual_diff - pts_diff) * 1000
                            pts_presentation_gaps.append(gap)
                    
                    last_pts = pts
                    
                    # First frame timing
                    if self.timestamps['first_frame_decoded'] is None:
                        self.timestamps['first_frame_decoded'] = frame_receive_time
                        first_frame_time = (frame_receive_time - 
                                          self.timestamps['connection_start']) * 1000
                        print(f"✅ First frame decoded: {first_frame_time:.2f}ms")
                        print(f"   Type: {'I-frame' if is_keyframe else 'P-frame'}")
                    
                    if is_keyframe and self.timestamps['first_iframe_decoded'] is None:
                        self.timestamps['first_iframe_decoded'] = frame_receive_time
                        first_iframe_time = (frame_receive_time - 
                                           self.timestamps['connection_start']) * 1000
                        print(f"✅ First I-frame decoded: {first_iframe_time:.2f}ms")
                    
                    # Verbose logging for first few frames
                    if verbose and frame_count <= 60:
                        elapsed = (frame_receive_time - start_time) * 1000
                        print(f"   Frame {frame_count}: "
                              f"{'I' if is_keyframe else 'P'}-frame, "
                              f"decode={decode_time:.2f}ms, "
                              f"elapsed={elapsed:.2f}ms, "
                              f"pts={pts}")
                    
                    self.last_frame_time = frame_receive_time
                    
                    # Track frame intervals
                    if len(self.frame_intervals) > 0:
                        interval = (frame_receive_time - self.last_frame_time) * 1000
                        self.frame_intervals.append(interval)
            
        except KeyboardInterrupt:
            print("\n⏸️  Interrupted by user")
        except Exception as e:
            print(f"\n❌ Error during analysis: {e}")
        
        finally:
            container.close()
        
        # Phase 3: Analysis
        print(f"\n{'='*70}")
        print("📊 LATENCY ANALYSIS")
        print("="*70)
        
        self.analyze_results(
            packet_count, frame_count, iframe_count,
            decode_latencies, pts_presentation_gaps
        )
    
    def analyze_results(self, packet_count, frame_count, iframe_count,
                       decode_latencies, pts_presentation_gaps):
        """Analyze and report latency sources"""
        
        # Connection latency
        print("\n1️⃣  Connection Latency:")
        if (self.timestamps['connection_complete'] and 
            self.timestamps['connection_start']):
            conn_latency = ((self.timestamps['connection_complete'] - 
                           self.timestamps['connection_start']) * 1000)
            print(f"   {conn_latency:.2f}ms")
            
            if conn_latency > 1000:
                print(f"   🚨 HIGH! Connection should be < 100ms on LAN")
                print(f"   → Check: DNS, firewall, server responsiveness")
        
        # First packet latency
        print("\n2️⃣  Time to First Packet:")
        if (self.timestamps['first_packet_received'] and 
            self.timestamps['connection_start']):
            first_packet_latency = ((self.timestamps['first_packet_received'] - 
                                    self.timestamps['connection_start']) * 1000)
            print(f"   {first_packet_latency:.2f}ms from start")
            
            if first_packet_latency > 500:
                print(f"   🚨 HIGH! Should be < 200ms")
                print(f"   → Check: Server processing, RTSP PLAY command delay")
        
        # Decode latency
        print("\n3️⃣  Decoder Latency:")
        if decode_latencies:
            avg_decode = sum(decode_latencies) / len(decode_latencies)
            max_decode = max(decode_latencies)
            print(f"   Average: {avg_decode:.2f}ms")
            print(f"   Maximum: {max_decode:.2f}ms")
            
            if avg_decode > 20:
                print(f"   🚨 HIGH! Decoding should be < 10ms per frame")
                print(f"   → Check: CPU load, decoder settings, resolution")
        
        # Packet arrival jitter
        print("\n4️⃣  Network Jitter:")
        if len(self.packet_times) > 10:
            avg_interval = sum(self.packet_times) / len(self.packet_times)
            max_interval = max(self.packet_times)
            min_interval = min(self.packet_times)
            jitter = max_interval - min_interval
            
            print(f"   Average packet interval: {avg_interval:.2f}ms")
            print(f"   Jitter (max-min): {jitter:.2f}ms")
            
            if jitter > 100:
                print(f"   🚨 HIGH JITTER! Indicates network buffering")
                print(f"   → Check: Network path, switch buffers, QoS")
        
        # PTS vs actual timing
        print("\n5️⃣  Presentation Timestamp Analysis:")
        if pts_presentation_gaps:
            avg_gap = sum(pts_presentation_gaps) / len(pts_presentation_gaps)
            max_gap = max(pts_presentation_gaps)
            
            print(f"   Average PTS drift: {avg_gap:.2f}ms")
            print(f"   Maximum PTS drift: {max_gap:.2f}ms")
            
            if avg_gap > 50:
                print(f"   🚨 DRIFT DETECTED! Frames delayed relative to PTS")
                print(f"   → This indicates accumulated buffering somewhere")
        
        # Frame statistics
        print(f"\n📈 Frame Statistics:")
        print(f"   Total packets: {packet_count}")
        print(f"   Total frames: {frame_count}")
        print(f"   I-frames: {iframe_count}")
        print(f"   P-frames: {frame_count - iframe_count}")
        
        if frame_count > 0:
            iframe_ratio = (iframe_count / frame_count) * 100
            print(f"   I-frame ratio: {iframe_ratio:.1f}%")
        
        # Root cause identification
        print(f"\n{'='*70}")
        print("🎯 ROOT CAUSE IDENTIFICATION")
        print("="*70)
        
        issues = []
        
        # Check each stage
        if (self.timestamps['connection_complete'] and 
            self.timestamps['connection_start']):
            conn_time = ((self.timestamps['connection_complete'] - 
                         self.timestamps['connection_start']) * 1000)
            if conn_time > 1000:
                issues.append(("Connection handshake", conn_time, 
                              "Server overloaded or network delay"))
        
        if (self.timestamps['first_packet_received'] and 
            self.timestamps['connection_complete']):
            play_latency = ((self.timestamps['first_packet_received'] - 
                            self.timestamps['connection_complete']) * 1000)
            if play_latency > 1000:
                issues.append(("RTSP PLAY → First packet", play_latency,
                              "Server needs time to start encoding/streaming"))
        
        if decode_latencies:
            avg_decode = sum(decode_latencies) / len(decode_latencies)
            if avg_decode > 20:
                issues.append(("Frame decoding", avg_decode,
                              "CPU overloaded or complex codec"))
        
        if pts_presentation_gaps:
            avg_gap = sum(pts_presentation_gaps) / len(pts_presentation_gaps)
            if avg_gap > 50:
                issues.append(("PTS timing drift", avg_gap,
                              "Hidden buffering in decoder or player"))
        
        if issues:
            print("\n🚨 LATENCY SOURCES FOUND:\n")
            for i, (source, latency, cause) in enumerate(issues, 1):
                print(f"{i}. {source}: +{latency:.0f}ms")
                print(f"   Cause: {cause}\n")
        else:
            print("\n✅ No major latency sources detected")
            print("   If you still see 5s latency in player:")
            print("   → Check player-side buffering (ffplay -fflags nobuffer)")
            print("   → Check OS network buffers")
            print("   → Enable player debug logging")
        
        print("="*70)


def main():
    parser = argparse.ArgumentParser(
        description='RTSP Latency Audit Tool - Find where latency hides',
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
Examples:
  # Basic audit (10 seconds)
  python3 rtsp_latency_audit.py rtsp://192.168.1.100:554/stream

  # Extended audit with verbose output
  python3 rtsp_latency_audit.py rtsp://server:554/stream -d 30 -v

  # Quick check
  python3 rtsp_latency_audit.py rtsp://camera:554/stream -d 5

What this tool checks:
  1. Connection handshake time (DNS, TCP, RTSP DESCRIBE/SETUP)
  2. Time from PLAY to first packet
  3. Decoder processing time per frame
  4. Network jitter (packet arrival variance)
  5. PTS drift (hidden buffering indicator)
        """
    )
    
    parser.add_argument('url', help='RTSP stream URL')
    parser.add_argument('-d', '--duration', type=int, default=10,
                       help='Analysis duration in seconds (default: 10)')
    parser.add_argument('-v', '--verbose', action='store_true',
                       help='Verbose frame-by-frame output')
    
    args = parser.parse_args()
    
    auditor = RTSPLatencyAuditor(args.url)
    auditor.audit_latency(duration=args.duration, verbose=args.verbose)


if __name__ == '__main__':
    main()