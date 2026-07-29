#!/usr/bin/env python
#
# Python signaling server example for libdatachannel
# Copyright (c) 2020 Paul-Louis Ageneau
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this
# file, You can obtain one at https://mozilla.org/MPL/2.0/.

import sys
import ssl
import json
import time
import asyncio
import logging
import websockets
from websockets.legacy.server import WebSocketServerProtocol, serve as ws_serve
from aiohttp import web


logger = logging.getLogger('websockets')
logger.setLevel(logging.INFO)
logger.addHandler(logging.StreamHandler(sys.stdout))


clients = {}       # client_id -> websocket
device_info = {}  # client_id -> {connected_at, path, type, last_seen}
INACTIVITY_TIMEOUT = 90  # seconds - disconnect if no activity
KEEPALIVE_INTERVAL = 30  # seconds - how often we ping each client
KEEPALIVE_PING_TIMEOUT = 10  # seconds - how long we wait for a pong


def load_turn_config():
    try:
        with open('cfg/turn.conf', 'r') as f:
            servers = json.load(f)
            if isinstance(servers, list):
                return servers
    except Exception as e:
        print('Failed to read turn.conf: {}'.format(e))
    return []


def load_version():
    try:
        with open('cfg/version', 'r') as f:
            return f.read().strip()
    except Exception:
        return 'unknown'


async def keepalive_loop(websocket, client_id, interval=KEEPALIVE_INTERVAL, timeout=KEEPALIVE_PING_TIMEOUT):
    """Explicit ping/pong keepalive with logging.

    Replaces websockets' built-in automatic ping_interval/ping_timeout
    mechanism (which has no user-visible hook for logging received
    ping/pong frames) with an explicit per-client task so we can log RTT
    and detect a dead connection ourselves.
    """
    try:
        while True:
            await asyncio.sleep(interval)
            try:
                pong_waiter = await websocket.ping()
                start = time.time()
                await asyncio.wait_for(pong_waiter, timeout=timeout)
                rtt_ms = (time.time() - start) * 1000
                if client_id in device_info:
                    device_info[client_id]['last_seen'] = time.time()
                    device_info[client_id]['rtt'] = rtt_ms
            except asyncio.TimeoutError:
                print('[{}] pong timeout after {}s, closing'.format(client_id, timeout))
                await websocket.close(1000, 'Ping timeout')
                break
    except (asyncio.CancelledError, websockets.exceptions.ConnectionClosed):
        pass  # normal on disconnect/shutdown


async def handle_websocket(websocket):
    client_id = None
    keepalive_task = None
    try:
        print('New connection from {} path={}'.format(websocket.remote_address, websocket.path))
        splitted = websocket.path.split('/')
        splitted.pop(0)
        client_id = splitted.pop(0)
        websocket._client_id = client_id
        print('Client {} connected'.format(client_id))

        device_type = 'consumer' if client_id.startswith('Player') else 'producer'
        clients[client_id] = websocket
        device_info[client_id] = {
            'connected_at': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()),
            'path': websocket.path,
            'type': device_type,
            'last_seen': time.time(),
            'rtt' : 0
        }

        # Start explicit keepalive/RTT logging for this client
        keepalive_task = asyncio.create_task(keepalive_loop(websocket, client_id))

        while True:
            try:
                data = await websocket.recv()
            except websockets.exceptions.ConnectionClosed as e:
                print('[{}] Connection closed: code={}, reason={}'.format(client_id, e.code, e.reason))
                break
            if client_id in device_info:
                device_info[client_id]['last_seen'] = time.time()
            print('[{}] << {}'.format(client_id, data))
            message = json.loads(data)

            # Handle clean disconnect message
            if message.get('type') == 'bye':
                print('[{}] sent bye, closing cleanly'.format(client_id))
                await websocket.close(1000, 'Client disconnected')
                break

            destination_id = message['id']
            destination_websocket = clients.get(destination_id)
            if destination_websocket:
                message['id'] = client_id
                if client_id.startswith('Player') and message.get('type') == 'request':
                    servers = load_turn_config()
                    if servers:
                        message['turn'] = servers
                        print('[{}] TURN padded to request for {}'.format(client_id, destination_id))
                data = json.dumps(message)
                # print('[{}] >> {}'.format(destination_id, data))
                send_start = time.time()
                await destination_websocket.send(data)
                send_elapsed = (time.time() - send_start) * 1000
                print('[{}] >> {} : sent in {:.2f}ms'.format(destination_id, data, send_elapsed))
            else:
                print('Client {} not found'.format(destination_id))
                error_response = {
                    "type": "error",
                    "msg": "Device offline"
                }
                await websocket.send(json.dumps(error_response))

    except websockets.exceptions.WebSocketException as e:
        print('[{}] WebSocket error: {}'.format(client_id, e))
    except Exception as e:
        print('[{}] Unexpected error: {}'.format(client_id, e))

    finally:
        if keepalive_task:
            keepalive_task.cancel()
        if client_id:
            del clients[client_id]
            device_info.pop(client_id, None)
            print('Client {} disconnected'.format(client_id))


# ── HTTP REST handlers ──────────────────────────────────────────────────────

async def http_devices(request):
    """GET /devices — return all currently connected clients.
    Query params:
      type  — filter by device type (e.g. ?type=producer)
    """
    type_filter = request.query.get('type', None)
    result = []
    for cid, info in device_info.items():
        if type_filter and info.get('type') != type_filter:
            continue
        result.append({
            'id': cid,
            'connected_at': info['connected_at'],
            'path': info['path'],
            'type': info['type'],
            'rtt': info['rtt'],
        })
    return web.json_response({
        'count': len(result),
        'devices': result,
    }, headers={'Access-Control-Allow-Origin': '*'})


async def http_health(request):
    """GET /health — liveness check."""
    return web.json_response({
        'status': 'ok',
        'clients': len(clients),
        'version': load_version(),
    }, headers={'Access-Control-Allow-Origin': '*'})


# ── Background tasks ───────────────────────────────────────────────────────

async def sweep_inactive_clients():
    """Periodically disconnect clients that haven't sent pings or messages."""
    while True:
        await asyncio.sleep(30)
        now = time.time()
        stale = [cid for cid, info in device_info.items()
                 if now - info.get('last_seen', 0) > INACTIVITY_TIMEOUT]
        for cid in stale:
            ws = clients.get(cid)
            if ws:
                print('[{}] Inactive for {}s, disconnecting'.format(cid, INACTIVITY_TIMEOUT))
                await ws.close(1000, 'Inactivity timeout')


# ── Main ─────────────────────────────────────────────────────────────────────

async def main():
    # Usage: ./server.py [[host:]port] [SSL certificate file] [http port]
    # Example: ./server.py 8000 cert.pem 8080
    endpoint_or_port = sys.argv[1] if len(sys.argv) > 1 else "8000"
    http_port        = int(sys.argv[2]) if len(sys.argv) > 2 else 8080
    ssl_cert         = sys.argv[3] if len(sys.argv) > 3 else None

    endpoint = endpoint_or_port if ':' in endpoint_or_port else "127.0.0.1:" + endpoint_or_port

    if ssl_cert:
        ssl_context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        ssl_context.load_cert_chain(ssl_cert)
    else:
        ssl_context = None

    host, port = endpoint.rsplit(':', 1)

    # WebSocket signaling server
    # Keepalive is handled explicitly per-client via keepalive_loop() above
    # (started in handle_websocket), so the built-in automatic ping is
    # disabled here to avoid double-pinging the same connection.
    ws_server = await ws_serve(
        handle_websocket,
        host,
        int(port),
        ssl=ssl_context,
        create_protocol=WebSocketServerProtocol,
        ping_interval=None,
        ping_timeout=None,
        close_timeout=10,
        max_queue=32,
        compression=None
    )

    # Start inactivity sweeper
    asyncio.create_task(sweep_inactive_clients())
    print('WebSocket signaling listening on {}'.format(endpoint))

    # HTTP REST server (always plain HTTP, sits behind nginx)
    app = web.Application()
    app.router.add_get('/devices', http_devices)
    app.router.add_get('/health',  http_health)
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, host, http_port)
    await site.start()
    print('HTTP REST API listening on {}:{}'.format(host, http_port))
    print('  GET http://{}:{}/devices  — list online devices'.format(host, http_port))
    print('  GET http://{}:{}/health   — health check'.format(host, http_port))

    await ws_server.wait_closed()


if __name__ == '__main__':
    asyncio.run(main())