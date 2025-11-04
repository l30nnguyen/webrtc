#!/usr/bin/env python3
"""
RTSP Connection Diagnostic Tool
Identifies bottlenecks causing high TTFF
"""

import socket
import time
import subprocess
import sys
from urllib.parse import urlparse

class RTSPDiagnostic:
    def __init__(self, rtsp_url):
        self.rtsp_url = rtsp_url
        self.parsed = urlparse(rtsp_url)
        self.host = self.parsed.hostname
        self.port = self.parsed.port or 554
    
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
    
    def run_full_diagnostic(self):
        """Run complete diagnostic suite"""
        print("="*70)
        print("🔍 RTSP Connection Diagnostic Tool")
        print("="*70)
        print(f"\n📡 URL: {self.rtsp_url}")
        print(f"🖥️  Host: {self.host}")
        print(f"🔌 Port: {self.port}")
        print(f"\n{'='*70}\n")
        
        results = {}
        
        # 1. DNS Resolution
        print("1️⃣  DNS Resolution Test")
        print("-" * 70)
        results['dns'] = self.test_dns()
        print()
        
        # 2. Network Connectivity
        print("2️⃣  Network Connectivity Test")
        print("-" * 70)
        results['ping'] = self.test_ping()
        print()
        
        # 3. Port Accessibility
        print("3️⃣  Port Accessibility Test")
        print("-" * 70)
        results['port'] = self.test_port()
        print()
        
        # 4. RTSP Handshake Breakdown
        print("4️⃣  RTSP Handshake Breakdown")
        print("-" * 70)
        results['rtsp'] = self.test_rtsp_handshake()
        print()
        
        # 5. First Packet Timing
        print("5️⃣  RTP First Packet Test")
        print("-" * 70)
        results['rtp'] = self.test_first_packet()
        print()
        
        # Summary and Recommendations
        print("="*70)
        print("📊 DIAGNOSTIC SUMMARY")
        print("="*70)
        self.print_summary(results)
        
    def test_dns(self):
        """Test DNS resolution time"""
        try:
            start = time.perf_counter()
            ip = socket.gethostbyname(self.host)
            elapsed = (time.perf_counter() - start) * 1000
            
            print(f"✅ DNS resolved: {self.host} → {ip}")
            print(f"⏱️  Time: {elapsed:.2f}ms")
            
            if elapsed < 10:
                print("✅ DNS resolution is FAST")
            elif elapsed < 100:
                print("⚠️  DNS resolution is SLOW (use IP address instead)")
            else:
                print("❌ DNS resolution is VERY SLOW (major issue)")
            
            return {'success': True, 'time': elapsed, 'ip': ip}
        except Exception as e:
            print(f"❌ DNS resolution FAILED: {e}")
            return {'success': False, 'error': str(e)}
    
    def test_ping(self):
        """Test network latency"""
        try:
            # Use IP if DNS resolved, otherwise hostname
            target = self.host
            
            print(f"Pinging {target} (10 packets)...")
            
            result = subprocess.run(
                ['ping', '-c', '4', target],
                capture_output=True,
                text=True,
                timeout=15
            )
            
            # Parse ping output
            output = result.stdout
            
            # Extract statistics
            if 'rtt min/avg/max' in output or 'round-trip min/avg/max' in output:
                # Linux/Mac format
                stats_line = [l for l in output.split('\n') if 'min/avg/max' in l][0]
                values = stats_line.split('=')[1].split()[0].split('/')
                min_rtt, avg_rtt, max_rtt = map(float, values[:3])
            else:
                print("⚠️  Could not parse ping output")
                return {'success': False}
            
            # Extract packet loss
            loss_line = [l for l in output.split('\n') if 'packet loss' in l][0]
            loss_pct = float(loss_line.split('%')[0].split()[-1])
            
            print(f"📊 Latency: min={min_rtt:.2f}ms avg={avg_rtt:.2f}ms max={max_rtt:.2f}ms")
            print(f"📦 Packet loss: {loss_pct}%")
            
            # Evaluate
            if loss_pct > 0:
                print(f"❌ PACKET LOSS DETECTED ({loss_pct}%) - Network issue!")
            elif avg_rtt < 1:
                print("✅ Latency is EXCELLENT (< 1ms)")
            elif avg_rtt < 10:
                print("✅ Latency is GOOD (< 10ms)")
            elif avg_rtt < 50:
                print("⚠️  Latency is ACCEPTABLE (< 50ms)")
            else:
                print(f"❌ Latency is HIGH ({avg_rtt:.2f}ms) - Network issue!")
            
            return {
                'success': True,
                'min': min_rtt,
                'avg': avg_rtt,
                'max': max_rtt,
                'loss': loss_pct
            }
            
        except subprocess.TimeoutExpired:
            print("❌ Ping TIMEOUT - Network unreachable!")
            return {'success': False, 'error': 'timeout'}
        except Exception as e:
            print(f"❌ Ping FAILED: {e}")
            return {'success': False, 'error': str(e)}
    
    def test_port(self):
        """Test if RTSP port is accessible"""
        try:
            start = time.perf_counter()
            sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            sock.settimeout(5)
            sock.connect((self.host, self.port))
            elapsed = (time.perf_counter() - start) * 1000
            sock.close()
            
            print(f"✅ Port {self.port} is OPEN")
            print(f"⏱️  TCP connection time: {elapsed:.2f}ms")
            
            if elapsed < 10:
                print("✅ TCP connection is FAST")
            elif elapsed < 100:
                print("⚠️  TCP connection is SLOW")
            else:
                print("❌ TCP connection is VERY SLOW")
            
            return {'success': True, 'time': elapsed}
            
        except socket.timeout:
            print(f"❌ Port {self.port} TIMEOUT - Firewall blocking?")
            return {'success': False, 'error': 'timeout'}
        except ConnectionRefusedError:
            print(f"❌ Port {self.port} REFUSED - Server not running?")
            return {'success': False, 'error': 'refused'}
        except Exception as e:
            print(f"❌ Port test FAILED: {e}")
            return {'success': False, 'error': str(e)}
    
    def test_rtsp_handshake(self):
        """Test RTSP handshake steps"""
        try:
            import av
            
            timings = {}
            
            # Step 1: OPTIONS (optional, many clients skip)
            print("📋 Testing RTSP handshake steps...")
            
            # Full connection with timing breakdown
            overall_start = time.perf_counter()            
            print("   1. Opening connection...")
            start = time.perf_counter()
            container = av.open(self.rtsp_url, options=self.get_options(), timeout=10.0)
            timings['open'] = (time.perf_counter() - start) * 1000
            print(f"      ✅ Connection opened: {timings['open']:.2f}ms")
            
            # Get stream info (triggers DESCRIBE)
            print("   2. Getting stream info (DESCRIBE)...")
            start = time.perf_counter()
            video_stream = next((s for s in container.streams if s.type == 'video'), None)
            timings['describe'] = (time.perf_counter() - start) * 1000
            
            if video_stream:
                print(f"      ✅ Stream info retrieved: {timings['describe']:.2f}ms")
                print(f"      📺 Codec: {video_stream.codec_context.name}")
                print(f"      📐 Resolution: {video_stream.width}x{video_stream.height}")
                print(f"      🎞️  FPS: {video_stream.average_rate}")
            else:
                print(f"      ❌ No video stream found!")
                return {'success': False}
            
            # Read first packet (PLAY command sent)
            print("   3. Reading first packet (SETUP + PLAY)...")
            start = time.perf_counter()
            
            for packet in container.demux(video_stream):
                timings['first_packet'] = (time.perf_counter() - start) * 1000
                print(f"      ✅ First packet received: {timings['first_packet']:.2f}ms")
                break
            
            container.close()
            
            total_time = (time.perf_counter() - overall_start) * 1000
            timings['total'] = total_time
            
            print(f"\n📊 Handshake Breakdown:")
            print(f"   Connection open:     {timings['open']:.2f}ms")
            print(f"   DESCRIBE:            {timings['describe']:.2f}ms")
            print(f"   SETUP + PLAY:        {timings['first_packet']:.2f}ms")
            print(f"   ─────────────────────────────────")
            print(f"   TOTAL:               {total_time:.2f}ms")
            
            # Identify bottleneck
            if timings['open'] > 1000:
                print("\n❌ BOTTLENECK: Connection opening is slow (>1s)")
                print("   → Check: Firewall, DNS, or server overload")
            elif timings['first_packet'] > 1000:
                print("\n❌ BOTTLENECK: SETUP/PLAY is slow (>1s)")
                print("   → Check: Server needs to start encoder, or buffer issues")
            elif total_time < 500:
                print("\n✅ Handshake is FAST (< 500ms)")
            else:
                print(f"\n⚠️  Handshake is SLOW ({total_time:.2f}ms)")
            
            return {'success': True, 'timings': timings}
            
        except Exception as e:
            print(f"❌ Handshake test FAILED: {e}")
            import traceback
            traceback.print_exc()
            return {'success': False, 'error': str(e)}
    
    def test_first_packet(self):
        """Test time to first RTP packet with detailed codec info"""
        try:
            import av
            
            print("Testing first packet arrival with frame analysis...")
            
            start = time.perf_counter()
            container = av.open(
                self.rtsp_url,
                options=self.get_options(),
                timeout=10.0
            )
            
            video_stream = next(s for s in container.streams if s.type == 'video')
            
            frame_count = 0
            first_iframe_time = None
            
            for packet in container.demux(video_stream):
                packet_time = (time.perf_counter() - start) * 1000
                
                for frame in packet.decode():
                    frame_count += 1
                    frame_time = (time.perf_counter() - start) * 1000
                    
                    frame_type = 'I' if frame.key_frame else 'P'
                    
                    print(f"   Frame #{frame_count}: {frame_type}-frame at {frame_time:.2f}ms")
                    
                    if frame.key_frame and first_iframe_time is None:
                        first_iframe_time = frame_time
                        print(f"   🔑 First I-frame found!")
                        break
                
                if first_iframe_time or frame_count >= 10:
                    break
            
            container.close()
            
            if first_iframe_time:
                print(f"\n✅ TTFF: {first_iframe_time:.2f}ms (after {frame_count} frames)")
            else:
                print(f"\n⚠️  No I-frame in first {frame_count} frames")
            
            return {
                'success': True,
                'ttff': first_iframe_time,
                'frames_to_iframe': frame_count
            }
            
        except Exception as e:
            print(f"❌ First packet test FAILED: {e}")
            return {'success': False, 'error': str(e)}
    
    def print_summary(self, results):
        """Print diagnostic summary and recommendations"""
        print()
        
        issues = []
        
        # Check DNS
        if results.get('dns', {}).get('success'):
            dns_time = results['dns']['time']
            if dns_time > 100:
                issues.append(("DNS Resolution", dns_time, "Use IP address instead of hostname"))
        
        # Check Network
        if results.get('ping', {}).get('success'):
            avg_rtt = results['ping']['avg']
            loss = results['ping']['loss']
            if loss > 0:
                issues.append(("Packet Loss", loss, "Fix network connectivity"))
            if avg_rtt > 50:
                issues.append(("Network Latency", avg_rtt, "Check network path, switches, or WiFi"))
        
        # Check RTSP
        if results.get('rtsp', {}).get('success'):
            timings = results['rtsp']['timings']
            if timings['open'] > 1000:
                issues.append(("Connection Open", timings['open'], "Server overloaded or firewall delay"))
            if timings.get('first_packet', 0) > 2000:
                issues.append(("SETUP/PLAY", timings['first_packet'], "Server slow to start streaming"))
        
        if issues:
            print("🚨 ISSUES FOUND:\n")
            for i, (issue, value, recommendation) in enumerate(issues, 1):
                print(f"{i}. {issue}: {value:.2f}ms")
                print(f"   💡 {recommendation}\n")
        else:
            print("✅ No major issues detected")
            print("   If TTFF is still high, check:")
            print("   - Server GOP size (should be 30-60 frames for 30fps)")
            print("   - Server encoding latency")
            print("   - Buffer sizes on server\n")
        
        print("="*70)


def main():
    if len(sys.argv) < 2:
        print("Usage: python3 rtsp_diagnostic.py <rtsp_url>")
        print("\nExample:")
        print("  python3 rtsp_diagnostic.py rtsp://192.168.1.100:554/stream")
        sys.exit(1)
    
    rtsp_url = sys.argv[1]
    
    diagnostic = RTSPDiagnostic(rtsp_url)
    diagnostic.run_full_diagnostic()


if __name__ == '__main__':
    main()
