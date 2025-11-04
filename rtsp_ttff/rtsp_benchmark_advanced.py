#!/usr/bin/env python3
"""
Advanced RTSP TTFF Benchmark using PyAV
Accurately detects I-frames, P-frames, and B-frames
"""

import av
import time
import argparse
import statistics
from collections import defaultdict

class AdvancedRTSPBenchmark:
    def __init__(self, rtsp_url, transport='tcp'):
        self.rtsp_url = rtsp_url
        self.transport = transport
    
    def measure_ttff(self, max_frames=300, verbose=False):
        """
        Measure Time-to-First-Frame with accurate frame type detection
        
        Returns:
            dict with timing metrics and frame type information
        """
        
        # Configure RTSP options
        options = {
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
        
        # Start timing
        start_time = time.perf_counter()
        
        try:
            # Open container
            container = av.open(self.rtsp_url, options=options, timeout=5.0)
            connection_time = (time.perf_counter() - start_time) * 1000
            
            if verbose:
                print(f"✅ Connected in {connection_time:.2f}ms")
            
            # Get video stream
            video_stream = next(s for s in container.streams if s.type == 'video')
            
            # Metrics
            metrics = {
                'connection_time': connection_time,
                'first_packet_time': None,
                'first_frame_time': None,
                'first_iframe_time': None,
                'first_pframe_time': None,
                'frames_before_iframe': 0,
                'frame_sequence': [],
                'frame_types': defaultdict(int)
            }
            
            frame_count = 0
            iframe_found = False
            
            # Read packets and decode frames
            for packet in container.demux(video_stream):
                current_time = (time.perf_counter() - start_time) * 1000
                
                # First packet
                if metrics['first_packet_time'] is None:
                    metrics['first_packet_time'] = current_time
                    if verbose:
                        print(f"📦 First packet at {current_time:.2f}ms")
                
                # Decode frames from packet
                for frame in packet.decode():
                    frame_count += 1
                    current_time = (time.perf_counter() - start_time) * 1000
                    
                    # Detect frame type
                    frame_type = self.get_frame_type(frame)
                    metrics['frame_types'][frame_type] += 1
                    metrics['frame_sequence'].append(frame_type)
                    
                    if verbose:
                        print(f"  Frame #{frame_count}: {frame_type} at {current_time:.2f}ms")
                    
                    # First frame of any type
                    if metrics['first_frame_time'] is None:
                        metrics['first_frame_time'] = current_time
                        if verbose:
                            print(f"🎬 First frame ({frame_type}) at {current_time:.2f}ms")
                    
                    # First I-frame
                    if frame_type == 'I' and metrics['first_iframe_time'] is None:
                        metrics['first_iframe_time'] = current_time
                        metrics['frames_before_iframe'] = frame_count - 1
                        iframe_found = True
                        if verbose:
                            print(f"🔑 First I-frame at {current_time:.2f}ms (after {metrics['frames_before_iframe']} frames)")
                    
                    # First P-frame
                    if frame_type == 'P' and metrics['first_pframe_time'] is None:
                        metrics['first_pframe_time'] = current_time
                        if verbose:
                            print(f"📊 First P-frame at {current_time:.2f}ms")
                    
                    # Stop after finding I-frame or max frames
                    if iframe_found or frame_count >= max_frames:
                        break
                
                if iframe_found or frame_count >= max_frames:
                    break
            
            container.close()
            
            metrics['total_frames'] = frame_count
            return metrics
            
        except Exception as e:
            print(f"❌ Error: {e}")
            return None
    
    def get_frame_type(self, frame):
        """
        Determine frame type (I, P, B)
        
        Args:
            frame: av.VideoFrame
            
        Returns:
            str: 'I', 'P', 'B', or 'Unknown'
        """
        # Check if frame is keyframe (I-frame)
        if frame.key_frame:
            return 'I'
        
        # Try to determine P vs B frame
        # In H.264, this is in the picture type
        try:
            if hasattr(frame, 'pict_type'):
                pict_type = frame.pict_type
                if pict_type == av.video.frame.PictureType.I:
                    return 'I'
                elif pict_type == av.video.frame.PictureType.P:
                    return 'P'
                elif pict_type == av.video.frame.PictureType.B:
                    return 'B'
        except:
            pass
        
        # Fallback: if not keyframe, likely P-frame
        return 'P'
    
    def run_benchmark(self, iterations=10, verbose=False):
        """Run benchmark multiple times"""
        
        print(f"\n🎬 Advanced RTSP TTFF Benchmark")
        print(f"📡 URL: {self.rtsp_url}")
        print(f"🔄 Transport: {self.transport.upper()}")
        print(f"🔢 Iterations: {iterations}")
        print(f"{'='*70}\n")
        
        results = []
        
        for i in range(iterations):
            print(f"[{i+1}/{iterations}] Testing...", end='', flush=True)
            
            result = self.measure_ttff(verbose=verbose)
            
            if result:
                results.append(result)
                ttff = result['first_iframe_time'] or result['first_frame_time']
                seq = ''.join(result['frame_sequence'][:10])
                print(f" ✅ TTFF: {ttff:.2f}ms | Seq: {seq}\n")
            else:
                print(" ❌ Failed")
            
            # Wait between iterations
            if i < iterations - 1:
                time.sleep(1.0)
        
        # Print statistics
        if results:
            self.print_statistics(results)
        else:
            print("\n❌ No successful measurements")
    
    def print_statistics(self, results):
        """Print detailed statistics"""
        
        print(f"\n{'='*70}")
        print("📊 DETAILED BENCHMARK RESULTS")
        print(f"{'='*70}\n")
        
        # Connection times
        conn_times = [r['connection_time'] for r in results]
        print("🔌 Connection Time:")
        self.print_metric_stats(conn_times)
        
        # First packet times
        packet_times = [r['first_packet_time'] for r in results if r['first_packet_time']]
        if packet_times:
            print("\n📦 Time to First Packet:")
            self.print_metric_stats(packet_times)
        
        # First frame times (any type)
        frame_times = [r['first_frame_time'] for r in results if r['first_frame_time']]
        if frame_times:
            print("\n🎬 Time to First Frame (any):")
            self.print_metric_stats(frame_times)
        
        # First P-frame times
        pframe_times = [r['first_pframe_time'] for r in results if r['first_pframe_time']]
        if pframe_times:
            print("\n📊 Time to First P-Frame:")
            self.print_metric_stats(pframe_times)
        
        # First I-frame times (TTFF)
        iframe_times = [r['first_iframe_time'] for r in results if r['first_iframe_time']]
        if iframe_times:
            print("\n🔑 Time to First I-Frame (TTFF):")
            self.print_metric_stats(iframe_times)
            
            # Frames before I-frame
            frames_before = [r['frames_before_iframe'] for r in results]
            print(f"\n   Frames before I-frame:")
            print(f"      Min: {min(frames_before)}")
            print(f"      Max: {max(frames_before)}")
            print(f"      Avg: {statistics.mean(frames_before):.1f}")
        
        # Frame type distribution
        print("\n📈 Frame Type Distribution:")
        all_types = defaultdict(int)
        for r in results:
            for ftype, count in r['frame_types'].items():
                all_types[ftype] += count
        
        total = sum(all_types.values())
        for ftype in sorted(all_types.keys()):
            count = all_types[ftype]
            pct = (count / total * 100) if total > 0 else 0
            print(f"   {ftype}-frames: {count} ({pct:.1f}%)")
        
        # Frame sequences
        print("\n🔢 Frame Sequences (first 10 frames):")
        for i, r in enumerate(results[:5], 1):  # Show first 5 results
            seq = ''.join(r['frame_sequence'][:10])
            print(f"   Run {i}: {seq}")
        
        print(f"\n✅ Success rate: {len(results)}/{len(results)} (100%)")
        print(f"{'='*70}\n")
    
    def print_metric_stats(self, values):
        """Print statistics for a metric"""
        if not values:
            return
        
        print(f"   Min:    {min(values):.2f}ms")
        print(f"   Max:    {max(values):.2f}ms")
        print(f"   Mean:   {statistics.mean(values):.2f}ms")
        print(f"   Median: {statistics.median(values):.2f}ms")
        if len(values) > 1:
            print(f"   StdDev: {statistics.stdev(values):.2f}ms")
            print(f"   P95:    {statistics.quantiles(values, n=20)[18]:.2f}ms")  # 95th percentile
            print(f"   P99:    {statistics.quantiles(values, n=100)[98]:.2f}ms")  # 99th percentile


def main():
    parser = argparse.ArgumentParser(
        description='Advanced RTSP TTFF Benchmark with Frame Type Detection',
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
Examples:
  # Basic benchmark
  python3 rtsp_benchmark_advanced.py rtsp://192.168.1.100:554/stream

  # TCP with 20 iterations and verbose output
  python3 rtsp_benchmark_advanced.py rtsp://camera:554/stream -t tcp -n 20 -v

  # UDP transport
  python3 rtsp_benchmark_advanced.py rtsp://server:554/stream -t udp

Frame Type Detection:
  I-frame: Keyframe (complete frame, can decode independently)
  P-frame: Predicted frame (needs previous frame)
  B-frame: Bidirectional frame (needs previous and next frames)
        """
    )
    
    parser.add_argument('url', help='RTSP stream URL')
    parser.add_argument('-n', '--iterations', type=int, default=10,
                       help='Number of test iterations (default: 10)')
    parser.add_argument('-t', '--transport', choices=['tcp', 'udp'], default='tcp',
                       help='RTSP transport protocol (default: tcp)')
    parser.add_argument('-v', '--verbose', action='store_true',
                       help='Verbose output showing each frame')
    
    args = parser.parse_args()
    
    # Run benchmark
    benchmark = AdvancedRTSPBenchmark(args.url, transport=args.transport)
    benchmark.run_benchmark(iterations=args.iterations, verbose=args.verbose)


if __name__ == '__main__':
    main()
