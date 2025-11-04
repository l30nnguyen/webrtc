# webrtc_gateway.py
import asyncio
import json
import logging
import os
from aiohttp import web
from aiortc import RTCPeerConnection, RTCSessionDescription, MediaStreamTrack
from aiortc.contrib.media import MediaBlackhole
import aiohttp_cors
import av

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("webrtc-gateway")

UDP_INPUT = os.environ.get("UDP_INPUT", "udp://127.0.0.1:8553")
# if your source is raw h264 stream, format='h264' is usually required
AV_INPUT_FORMAT = os.environ.get("AV_INPUT_FORMAT", "h264")


class H264DecodeTrack(MediaStreamTrack):
    """
    A MediaStreamTrack that reads from UDP H.264 stream (via PyAV),
    decodes into AV frames and yields aiortc-compatible VideoFrame objects.
    """

    kind = "video"

    def __init__(self, source_url=UDP_INPUT, input_format=AV_INPUT_FORMAT):
        super().__init__()  # don't forget this
        self.source_url = source_url
        self.input_format = input_format
        self.container = None
        self.stream = None
        self._task = None
        self._queue = asyncio.Queue(maxsize=10)  # decoded frames buffer
        self._closed = False
        # start reader task
        self._task = asyncio.create_task(self._reader())

    async def _reader(self):
        # open container (blocking operations called in threadpool by av.open)
        logger.info("Opening AV input %s (format=%s)", self.source_url, self.input_format)
        try:
            self.container = av.open(self.source_url, mode="r", format=self.input_format)
        except Exception as e:
            logger.exception("Failed to open input: %s", e)
            return

        # pick first video stream
        self.stream = next((s for s in self.container.streams if s.type == "video"), None)
        if self.stream is None:
            logger.error("No video stream found in %s", self.source_url)
            return

        logger.info("Starting decode loop from %s", self.source_url)
        try:
            for packet in self.container.demux(self.stream):
                if self._closed:
                    break
                for frame in packet.decode():
                    # put frame (av.VideoFrame) into queue (drop if queue full)
                    try:
                        self._queue.put_nowait(frame)
                    except asyncio.QueueFull:
                        # drop oldest to keep fresh
                        try:
                            _ = self._queue.get_nowait()
                            self._queue.put_nowait(frame)
                        except Exception:
                            pass
                # allow cooperative scheduling
                await asyncio.sleep(0)
        except Exception:
            logger.exception("Exception during decode loop")
        finally:
            try:
                self.container.close()
            except Exception:
                pass
            logger.info("Decode reader finished")

    async def recv(self):
        """
        Called by aiortc to get the next av.VideoFrame (wrapped as aiortc VideoFrame).
        We will pop from queue; if empty, wait a short time and return a black frame.
        """
        if self._closed:
            raise asyncio.CancelledError

        # wait for a decoded frame
        try:
            frame = await asyncio.wait_for(self._queue.get(), timeout=2.0)
        except asyncio.TimeoutError:
            # return a blank frame to keep the pipeline alive
            width, height = 1280, 720
            new_frame = av.VideoFrame(width, height, "yuv420p")
            pts, time_base = None, None
            return new_frame

        # convert to aiortc frame if needed (av.VideoFrame is accepted)
        return frame

    async def stop(self):
        self._closed = True
        if self._task:
            self._task.cancel()
            try:
                await self._task
            except Exception:
                pass
        await super().stop()

pcs = set()

async def offer(request):
    """
    Accepts either:
    - JSON: { "sdp": "<offer sdp>", "type": "offer" }
    - Plain text: raw SDP (Content-Type: text/plain)
    """
    content_type = request.headers.get("Content-Type", "")
    if "application/json" in content_type:
        data = await request.json()
        offer_sdp = data.get("sdp")
        offer_type = data.get("type", "offer")
    elif "text/plain" in content_type:
        offer_sdp = await request.text()
        offer_type = "offer"
    else:
        return web.Response(status=400, text="Unsupported Content-Type")

    if not offer_sdp:
        return web.Response(status=400, text="Missing SDP")

    pc = RTCPeerConnection()
    pcs.add(pc)
    logger.info("Created PeerConnection %s", pc)

    # Attach sink to print remote tracks if any
    media_blackhole = MediaBlackhole()

    @pc.on("connectionstatechange")
    async def on_connectionstatechange():
        logger.info("Connection state %s", pc.connectionState)
        if pc.connectionState == "failed" or pc.connectionState == "closed":
            await pc.close()
            pcs.discard(pc)

    # Add local video track (decoded H264 -> raw frames)
    video_track = H264DecodeTrack()
    pc.addTrack(video_track)

    # set remote description
    offer = RTCSessionDescription(sdp=offer_sdp, type=offer_type)
    await pc.setRemoteDescription(offer)

    # create answer
    answer = await pc.createAnswer()
    await pc.setLocalDescription(answer)

    # return answer SDP
    response = {
        "sdp": pc.localDescription.sdp,
        "type": pc.localDescription.type
    }
    print(json.dumps(response))
    return web.Response(content_type="application/json", text=json.dumps(response))


async def on_shutdown(app):
    # close all peer connections
    coros = [pc.close() for pc in pcs]
    await asyncio.gather(*coros)
    pcs.clear()


def main():
    app = web.Application()
    app.router.add_post("/offer", offer)
    app.on_shutdown.append(on_shutdown)
    cors = aiohttp_cors.setup(app, defaults={
        "*": aiohttp_cors.ResourceOptions(
            allow_credentials=True,
            expose_headers="*",
            allow_headers="*",
        )
    })
    for route in list(app.router.routes()):
        cors.add(route)
    web.run_app(app, port=1988)


if __name__ == "__main__":
    main()