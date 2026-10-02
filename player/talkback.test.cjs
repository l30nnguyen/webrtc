// Run: node --test --test-isolation=none server/player/talkback.test.cjs
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const html = fs.readFileSync(__dirname + '/ws.html', 'utf8');
const source = html.slice(html.indexOf('        class TalkbackController'), html.indexOf('        const talkback ='));
function setup(capture) {
    const timers = new Map(); let nextTimer = 0;
    const context = vm.createContext({ navigator: { mediaDevices: { getUserMedia() {} } }, window: { isSecureContext: true },
        Date, setInterval(fn) { timers.set(++nextTimer, fn); return nextTimer; }, clearInterval(id) { timers.delete(id); } });
    vm.runInContext(source + '\nthis.Controller = TalkbackController;', context);
    const audio = { muted: false }, states = [], messages = [];
    const controller = new context.Controller(audio, s => states.push(s), capture);
    const sender = { async replaceTrack(track) { this.track = track; } };
    const transceiver = { mid: 'talkback', receiver: { track: { kind: 'audio' } }, sender, direction: 'recvonly' };
    const peer = { getTransceivers: () => [transceiver], iceConnectionState: 'connected' };
    const channel = { readyState: 'open', send: text => messages.push(JSON.parse(text)) };
    controller.configure(peer); controller.connection(peer); controller.setChannel(peer, channel);
    return { controller, audio, states, messages, sender, peer, channel, timers, transceiver };
}
function mic() {
    const track = { enabled: true, stopped: false, stop() { this.stopped = true; } };
    return { track, getAudioTracks: () => [track], getTracks: () => [track] };
}
test('answer reserves send direction without requesting microphone; legacy camera unavailable', () => {
    const s = setup(() => { throw Error('must not capture'); });
    assert.equal(s.transceiver.direction, 'sendonly');
    assert.equal(s.controller.track, null);
    s.controller.configure({ getTransceivers: () => [] });
    assert.equal(s.states.at(-1).enable, false);
    assert.match(s.states.at(-1).message, /unavailable/);
});
test('grant enables PCMA sender, release restores mute and stale grant cannot restart', async () => {
    const stream = mic(), s = setup(async () => stream);
    await s.controller.enable();
    assert.equal(s.sender.track.enabled, false);
    s.controller.press();
    const requestId = s.controller.requestId;
    assert.equal(stream.track.enabled, false);
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({type:'ptt',action:'granted',requestId}));
    assert.equal(stream.track.enabled, true); assert.equal(s.audio.muted, true);
    s.controller.release();
    assert.equal(stream.track.enabled, false); assert.equal(s.audio.muted, false);
    assert.equal(s.messages.at(-1).action, 'stop'); assert.equal(s.timers.size, 0);
    s.controller.messageFrom(s.peer,s.channel,JSON.stringify({type:'ptt',action:'granted',requestId}));
    assert.equal(stream.track.enabled, false);
    for (const text of ['PING 42', 'null', '42', '[]']) assert.equal(s.controller.messageFrom(s.peer,s.channel,text), false);
});
test('pending permission is discarded and stopped after session reset', async () => {
    let resolve; const stream = mic(), s = setup(() => new Promise(r => resolve = r));
    const enabling = s.controller.enable(); s.controller.reset(); resolve(stream); await enabling;
    assert.equal(stream.track.stopped, true); assert.equal(s.sender.track, undefined); assert.equal(s.controller.track, null);
});
test('pending replaceTrack cannot install microphone into next generation', async () => {
    const stream = mic(), s = setup(async () => stream); let finish;
    s.sender.replaceTrack = () => new Promise(r => finish = r);
    const enabling = s.controller.enable(); await new Promise(setImmediate);
    s.controller.reset(); assert.equal(stream.track.stopped, true); finish(); await enabling;
    assert.equal(stream.track.stopped, true); assert.equal(s.controller.track, null);
});
test('disconnect stops ownership; terminal reset stops capture; previous muted state survives', async () => {
    const stream = mic(), s = setup(async () => stream); await s.controller.enable();
    s.audio.muted = true; s.controller.press();
    s.controller.messageFrom(s.peer,s.channel,JSON.stringify({type:'ptt',action:'granted',requestId:s.controller.requestId}));
    s.peer.iceConnectionState = 'disconnected'; s.controller.connection(s.peer);
    assert.equal(stream.track.enabled, false); assert.equal(s.audio.muted, true); assert.equal(s.timers.size, 0);
    s.controller.reset(); assert.equal(stream.track.stopped, true);
});
test('busy rejects ownership and permission denial remains retryable', async () => {
    const s = setup(async () => { throw Error('denied'); }); await s.controller.enable();
    assert.match(s.states.at(-1).message, /denied/); assert.equal(s.states.at(-1).enable, true);
    s.controller.capture = async () => mic(); await s.controller.enable(); s.controller.press();
    s.controller.messageFrom(s.peer,s.channel,JSON.stringify({type:'ptt',action:'busy',requestId:s.controller.requestId}));
    assert.equal(s.controller.track.enabled, false); assert.equal(s.controller.requestId, null);
});
test('whole inline player script parses', () => {
    const script = html.match(/<script>\s*([\s\S]*?)<\/script>/)[1]; new vm.Script(script);
});

test('missing grant acknowledgement times out and microphone removal releases owner', async () => {
    const stream = mic(), s = setup(async () => stream); await s.controller.enable(); s.controller.press();
    s.controller.lastGrant = Date.now() - 3000;
    [...s.timers.values()][0]();
    assert.equal(s.controller.requestId, null); assert.equal(stream.track.enabled, false);
    assert.match(s.states.at(-1).message, /timed out/);
    s.controller.press(); stream.track.onended();
    assert.equal(s.controller.track, null); assert.equal(s.messages.at(-1).action, 'stop');
});

test('camera diagnostic survives pointerup, lost capture and late stopped reply', async () => {
    const stream = mic(), s = setup(async () => stream), reports = [];
    s.controller.report = message => reports.push(message);
    await s.controller.enable(); s.controller.press();
    const requestId = s.controller.requestId;
    const detail = 'Speaker socket unavailable <img src=x onerror=alert(1)>';
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'ptt', action: 'error', requestId,
        code: 'sink_unavailable', message: detail }));
    assert.equal(s.controller.track.enabled, false);
    assert.equal(s.states.at(-1).message, detail + ' [sink_unavailable]');
    s.controller.release(); s.controller.release();
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'ptt', action: 'stopped', requestId }));
    assert.equal(s.states.at(-1).message, detail + ' [sink_unavailable]');
    assert.equal(reports.length, 1); assert.ok(reports[0].includes(detail));
});
test('duplicate presses/releases produce one start/stop and keepalive uses matching ID', async () => {
    const s = setup(async () => mic()); await s.controller.enable();
    s.controller.press(); s.controller.press();
    const requestId = s.controller.requestId;
    [...s.timers.values()][0]();
    s.controller.release(); s.controller.release();
    assert.deepEqual(s.messages.map(m => m.action), ['start', 'keepalive', 'stop']);
    assert.ok(s.messages.every(m => m.type === 'ptt' && m.requestId === requestId));
    assert.ok(requestId.length > 0 && requestId.length <= 128);
    assert.equal(s.timers.size, 0);
});

test('camera message containing its diagnostic code is not duplicated', async () => {
    const s = setup(async () => mic()); await s.controller.enable(); s.controller.press();
    const message = 'Talkback sink refused [sink_refused] (connect, errno=111)';
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type:'ptt', action:'error',
        requestId:s.controller.requestId, code:'sink_refused', message }));
    assert.equal(s.states.at(-1).message, message);
});
