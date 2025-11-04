#!/bin/bash

python src/rtsp_diagnostic.py rtsp://127.0.0.1:16667/blinkhd
python src/rtsp_benchmark_advanced.py \
  rtsp://127.0.0.1:16667/blinkhd \
  -n 10 \
  -t tcp \
  -v > benchmark_results.log

python src/rtsp_latency_audit.py rtsp://127.0.0.1:16667/blinkhd -d 10 -v