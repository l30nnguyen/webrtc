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
from aiohttp import web


logger = logging.getLogger('websockets')
logger.setLevel(logging.INFO)
logger.addHandler(logging.StreamHandler(sys.stdout))

clients = {}       # client_id -> websocket
device_info = {}  # client_id -> {connected_at, path, type}

def load_turn_config():
    try:
        with open('turn.conf', 'r') as f:
            servers = json.load(f)
            if isinstance(servers, list):
                return servers
    except Exception as e:
        print('Failed to read turn.conf: {}'.format(e))
    return []


async def handle_websocket(websocket):
    client_id = None
    try:
        splitted = websocket.request.path.split('/')
        splitted.pop(0)
        client_id = splitted.pop(0)
        print('Client {} connected'.format(client_id))

        device_type = 'consumer' if client_id.startswith('Player') else 'producer'
        clients[client_id] = websocket
        device_info[client_id] = {
            'connected_at': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()),
            'path': websocket.request.path,
            'remote': str(websocket.remote_address),
            'type': device_type,
        }
        while True:
            data = await websocket.recv()
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
                print('[{}] >> {}'.format(destination_id, data))
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

    except Exception as e:
        print(e)

    finally:
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
            'remote': info['remote'],
            'path': info['path'],
            'type': info['type'],
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
        'version': '2.0.0',
    }, headers={'Access-Control-Allow-Origin': '*'})


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
    ws_server = await websockets.serve(handle_websocket, host, int(port), ssl=ssl_context)
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