// Run: node --test server/player/recording.test.cjs
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const html = fs.readFileSync(__dirname + '/ws.html', 'utf8');
const source = html.slice(html.indexOf('        class RecordingController'), html.indexOf('        const talkback ='));

function setup() {
    let now = 1_000_000;
    const timers = new Map(); let nextTimer = 0;
    const context = vm.createContext({ Date: { now: () => now }, performance: { now: () => now }, Map, Number, JSON,
        setInterval(fn) { timers.set(++nextTimer, fn); return nextTimer; },
        clearInterval(id) { timers.delete(id); } });
    vm.runInContext(source + '\nthis.Controller = RecordingController;', context);
    const states = [], reports = [], messages = [];
    const controller = new context.Controller(state => states.push(state), message => reports.push(message));
    const peer = { iceConnectionState: 'connected' };
    const channel = { readyState: 'open', send: text => messages.push(JSON.parse(text)) };
    controller.configure(peer);
    controller.connection(peer);
    controller.setChannel(peer, channel);
    return { controller, peer, channel, states, reports, messages, timers,
        advance(ms) { now += ms; for (const fn of timers.values()) fn(); } };
}

test('list, select and play recording use the active data channel', () => {
    const s = setup();
    const listId = s.controller.load();
    assert.equal(s.messages.at(-1).type, 'recording');
    assert.equal(s.messages.at(-1).action, 'list');
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'recording', action: 'list', requestId: listId,
        files: [{ path: '2026-10-01/clip.mp4' }, { path: 'nested/other.mp4' }, { path: 42 }] }));
    assert.deepEqual(s.controller.files.map(file => file.path), ['2026-10-01/clip.mp4', 'nested/other.mp4']);
    assert.equal(s.controller.selectedPath, '2026-10-01/clip.mp4');
    s.controller.select('nested/other.mp4');
    const playId = s.controller.play();
    assert.deepEqual(s.messages.at(-1), { type: 'recording', action: 'play', requestId: playId, path: 'nested/other.mp4' });
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'recording', action: 'playing', requestId: playId,
        path: 'nested/other.mp4', durationMs: 31500 }));
    assert.equal(s.controller.playing, true);
    assert.match(s.states.at(-1).message, /0:32 remaining/);
    assert.equal(s.timers.size, 1);
    s.advance(1200);
    assert.match(s.states.at(-1).message, /0:31 remaining/);
    s.advance(60_000);
    assert.equal(s.controller.playing, true);
    assert.match(s.states.at(-1).message, /0:00 remaining/);
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'recording', action: 'ended', requestId: playId }));
    assert.equal(s.timers.size, 0);
});

test('countdown timers stop for stop, errors, reset, and disconnect', () => {
    const s = setup();
    const listId = s.controller.load();
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'recording', action: 'list', requestId: listId,
        files: [{ path: 'clip.mp4' }] }));
    const start = () => {
        const id = s.controller.play();
        s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'recording', action: 'playing', requestId: id,
            path: 'clip.mp4', durationMs: 1000 }));
        assert.equal(s.timers.size, 1);
        return id;
    };
    start();
    s.controller.stop();
    assert.equal(s.timers.size, 0);
    const errorId = start();
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'recording', action: 'error', requestId: errorId,
        message: 'failed' }));
    assert.equal(s.timers.size, 0);
    start();
    s.controller.reset();
    assert.equal(s.timers.size, 0);
    s.controller.configure(s.peer); s.controller.connection(s.peer); s.controller.setChannel(s.peer, s.channel);
    const listAgain = s.controller.load();
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'recording', action: 'list', requestId: listAgain,
        files: [{ path: 'clip.mp4' }] }));
    start();
    s.peer.iceConnectionState = 'disconnected'; s.controller.connection(s.peer);
    assert.equal(s.timers.size, 0);
});

test('late replies cannot restart playback after return to live or reset', () => {
    const s = setup();
    const listId = s.controller.load();
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'recording', action: 'list', requestId: listId,
        files: [{ path: 'clip.mp4' }] }));
    s.controller.select('clip.mp4');
    const playId = s.controller.play();
    const stopId = s.controller.stop();
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'recording', action: 'playing', requestId: playId }));
    assert.equal(s.controller.playing, false);
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'recording', action: 'live', requestId: stopId }));
    assert.equal(s.controller.playing, false);
    s.controller.reset();
    assert.equal(s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'recording', action: 'playing', requestId: playId })), true);
    assert.equal(s.controller.playing, false);
    assert.equal(s.controller.files.length, 0);
});

test('only the latest media command may change playback state', () => {
    const s = setup();
    const listId = s.controller.load();
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'recording', action: 'list', requestId: listId,
        files: [{ path: 'first.mp4' }, { path: 'second.mp4' }] }));
    const firstPlay = s.controller.play();
    s.controller.select('second.mp4');
    const secondPlay = s.controller.play();
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'recording', action: 'playing', requestId: firstPlay,
        path: 'first.mp4' }));
    assert.equal(s.controller.playing, false);
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'recording', action: 'playing', requestId: secondPlay,
        path: 'second.mp4' }));
    assert.equal(s.controller.playing, true);
    const stopId = s.controller.stop();
    assert.equal(s.controller.playing, false);
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'recording', action: 'ended', requestId: secondPlay }));
    assert.notEqual(s.states.at(-1).message, 'Recording ended — live view');
    const thirdPlay = s.controller.play();
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'recording', action: 'live', requestId: stopId }));
    assert.notEqual(s.states.at(-1).message, 'Live view');
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'recording', action: 'playing', requestId: thirdPlay,
        path: 'second.mp4' }));
    assert.equal(s.controller.playing, true);
});

test('errors and non-recording payloads are safely scoped to the request', () => {
    const s = setup();
    const listId = s.controller.load();
    assert.equal(s.controller.messageFrom(s.peer, s.channel, 'PING 10'), false);
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'recording', action: 'error', requestId: 'old', message: 'old error' }));
    assert.notEqual(s.states.at(-1).message, 'old error');
    const detail = 'File rejected <img src=x onerror=alert(1)>';
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'recording', action: 'error', requestId: listId, message: detail }));
    assert.equal(s.states.at(-1).message, detail);
    assert.equal(s.reports.length, 1);
});

test('whole inline player script parses', () => {
    const script = html.match(/<script>\s*([\s\S]*?)<\/script>/)[1];
    new vm.Script(script);
});

const push = (s, message) => s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'stream_state', ...message }));
const loadClip = s => {
    const listId = s.controller.load();
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'recording', action: 'list', requestId: listId,
        files: [{ path: 'clip.mp4' }] }));
};

test('play sends onEnd only when hold is selected', () => {
    const s = setup();
    loadClip(s);
    const first = s.controller.play();
    assert.deepEqual(s.messages.at(-1), { type: 'recording', action: 'play', requestId: first, path: 'clip.mp4' });
    s.controller.setOnEnd('hold');
    const second = s.controller.play();
    assert.deepEqual(s.messages.at(-1), { type: 'recording', action: 'play', requestId: second, path: 'clip.mp4', onEnd: 'hold' });
    s.controller.setOnEnd('bogus');
    s.controller.play();
    assert.equal('onEnd' in s.messages.at(-1), false);
});

test('stream_state drives mode, countdown and the return to live', () => {
    const s = setup();
    assert.equal(s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'stream_state', mode: 'live' })), true);
    assert.equal(s.controller.mode, 'live');
    assert.equal(s.states.at(-1).message, 'Live view');
    push(s, { mode: 'recording', path: 'clip.mp4', durationMs: 60000, positionMs: 12000, onEnd: 'live' });
    assert.equal(s.controller.mode, 'recording');
    assert.equal(s.controller.playing, true);
    assert.match(s.states.at(-1).message, /0:48 remaining/);
    push(s, { mode: 'recording', path: 'clip.mp4', durationMs: 60000, positionMs: 20000, onEnd: 'live' });
    assert.match(s.states.at(-1).message, /0:40 remaining/);
    assert.equal(s.timers.size, 1);
    push(s, { mode: 'switching_to_live', reason: 'ended' });
    assert.equal(s.controller.playing, false);
    assert.equal(s.timers.size, 0);
    assert.match(s.states.at(-1).message, /returning to live/);
    push(s, { mode: 'live' });
    assert.equal(s.states.at(-1).message, 'Live view');
    assert.equal(s.states.at(-1).mode, 'live');
});

test('periodic live resync keeps the status text and stale channels are ignored', () => {
    const s = setup();
    loadClip(s);
    assert.match(s.states.at(-1).message, /1 recording loaded/);
    push(s, { mode: 'live' });
    const shown = s.states.at(-1).message;
    push(s, { mode: 'live' });
    assert.equal(s.states.at(-1).message, shown);
    const count = s.states.length;
    s.controller.messageFrom(s.peer, { readyState: 'open' }, JSON.stringify({ type: 'stream_state', mode: 'recording' }));
    assert.equal(s.states.length, count);
    assert.equal(s.controller.mode, 'live');
    push(s, { mode: 'bogus' });
    assert.equal(s.controller.mode, 'live');
});

test('hold keeps recording mode until the user returns to live', () => {
    const s = setup();
    loadClip(s);
    s.controller.setOnEnd('hold');
    const id = s.controller.play();
    push(s, { mode: 'recording', path: 'clip.mp4', durationMs: 5000, positionMs: 0, onEnd: 'hold' });
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'recording', action: 'playing', requestId: id,
        path: 'clip.mp4', durationMs: 5000 }));
    assert.equal(s.timers.size, 1);
    push(s, { mode: 'recording_ended', path: 'clip.mp4', durationMs: 5000, positionMs: 5000, onEnd: 'hold', holdTimeoutMs: 60000, reason: 'ended' });
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'recording', action: 'ended', requestId: id, onEnd: 'hold' }));
    assert.equal(s.controller.mode, 'recording_ended');
    assert.equal(s.timers.size, 0);
    assert.match(s.states.at(-1).message, /holding last frame/);
    const stopId = s.controller.stop();
    assert.equal(s.messages.at(-1).action, 'stop');
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'recording', action: 'live', requestId: stopId }));
    push(s, { mode: 'switching_to_live', reason: 'stopped' });
    push(s, { mode: 'live' });
    assert.equal(s.controller.mode, 'live');
    assert.equal(s.states.at(-1).message, 'Live view');
});

test('device ping: JSON and legacy text are both answered in the format they arrived in', () => {
    const start = html.indexOf('        function parseDevicePing');
    const end = html.indexOf('        // Recording playback is scoped');
    const context = vm.createContext({ Number, JSON });
    vm.runInContext(html.slice(start, end) + '\nthis.parse = parseDevicePing; this.pong = devicePong;', context);

    const json = context.parse('{"type":"ping","seq":345,"rttMs":12,"version":"2.0.14"}');
    assert.deepEqual({ ...json }, { json: true, seq: 345, rttMs: 12, version: '2.0.14' });
    assert.deepEqual(JSON.parse(context.pong(json, 99)), { type: 'pong', seq: 345, ts: 99 });

    const text = context.parse('PING 12 346 2.0.12');
    assert.deepEqual({ ...text }, { json: false, seq: '346', rttMs: 12, version: '2.0.12' });
    assert.equal(context.pong(text, 99), 'PONG 99 346');
    assert.equal(context.parse('ping 0 7').version, '');

    for (const bad of ['{"type":"ping"}', '{"type":"ping","seq":-1}', '{"type":"ping","seq":"3"}', '{"type":"pong","seq":1}',
        '{"type":"ping","seq":', 'PING 1', 'hello world', '{"type":"recording"}']) assert.equal(context.parse(bad), null, bad);
    assert.equal(context.parse('{"type":"ping","seq":1,"rttMs":"x"}').rttMs, null);
});
