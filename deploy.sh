#!/bin/bash

# Compile Go server to root folder
echo "Building Go server..."
cd src/go
go build -o ../../signaling-server src/signaling-server.go
cd ../..

if [ $? -ne 0 ]; then
    echo "Build failed!"
    exit 1
fi

echo "Build successful!"

# Start/restart with PM2
echo "Starting PM2..."
pm2 start pm2_start.json --only webrtc_go --update-env

echo "Deployment complete!"
