// Run: node --test server/player/lens.test.cjs
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const html = fs.readFileSync(__dirname + '/ws.html', 'utf8');
const source = html.slice(html.indexOf('        class LensController'), html.indexOf('        // PTT controller owns'));

function setup({ connect = true, open = true } = {}) {
    const context = vm.createContext({ Date, JSON, Array });
    vm.runInContext(source + '\nthis.Controller = LensController;', context);
    const states = [], messages = [];
    const controller = new context.Controller(state => states.push(state));
    const peer = { iceConnectionState: 'connected' };
    const channel = { readyState: open ? 'open' : 'connecting', send: text => messages.push(JSON.parse(text)) };
    controller.configure(peer);
    if (connect) controller.connection(peer);
    controller.setChannel(peer, channel);
    const lensState = (active, available = true) => JSON.stringify({ type: 'lens_state', lenses: [
        { lens: 0, available: true, active: true }, { lens: 1, available, active }] });
    return { controller, peer, channel, states, messages, lensState };
}

test('a single-lens camera never shows lens 2', () => {
    const s = setup();
    assert.equal(s.states.at(-1).available, false);
    s.controller.messageFrom(s.peer, s.channel, s.lensState(false, false));
    assert.equal(s.states.at(-1).available, false);
    assert.equal(s.controller.start(), null);
    assert.equal(s.messages.length, 0);
    assert.equal(s.states.at(-1).message, 'Lens 2 is unavailable');
});

test('start and stop go over the data channel and follow the camera replies', () => {
    const s = setup();
    s.controller.messageFrom(s.peer, s.channel, s.lensState(false));
    assert.equal(s.states.at(-1).available, true);
    assert.equal(s.states.at(-1).active, false);
    assert.equal(s.states.at(-1).ready, true);
    const id = s.controller.toggle();
    assert.deepEqual(s.messages.at(-1), { type: 'live', lens: 1, action: 'start', requestId: id });
    assert.equal(s.states.at(-1).busy, true);
    assert.equal(s.controller.toggle(), null, 'no second request while one is pending');
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'live', lens: 1, action: 'started', requestId: id }));
    s.controller.messageFrom(s.peer, s.channel, s.lensState(true));
    assert.equal(s.states.at(-1).active, true);
    assert.equal(s.states.at(-1).busy, false);
    assert.equal(s.states.at(-1).message, 'Waiting for lens 2 video');
    s.controller.videoFrame();
    assert.equal(s.states.at(-1).message, '');
    const stopId = s.controller.toggle();
    assert.deepEqual(s.messages.at(-1), { type: 'live', lens: 1, action: 'stop', requestId: stopId });
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'live', lens: 1, action: 'stopped', requestId: stopId }));
    assert.equal(s.states.at(-1).active, false);
    assert.equal(s.states.at(-1).busy, false);
});

test('an error reply is shown and clears the pending request', () => {
    const s = setup();
    s.controller.messageFrom(s.peer, s.channel, s.lensState(false));
    const id = s.controller.start();
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'live', lens: 1, action: 'error', requestId: id, message: 'this camera has no second lens' }));
    assert.equal(s.states.at(-1).message, 'this camera has no second lens');
    assert.equal(s.states.at(-1).busy, false);
    assert.equal(s.states.at(-1).active, false);
});

test('replies for another request, peer or channel are ignored', () => {
    const s = setup();
    s.controller.messageFrom(s.peer, s.channel, s.lensState(false));
    const id = s.controller.start();
    s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'live', lens: 1, action: 'started', requestId: 'other' }));
    assert.equal(s.states.at(-1).active, false);
    assert.equal(s.states.at(-1).busy, true);
    assert.equal(s.controller.messageFrom({}, s.channel, JSON.stringify({ type: 'live', requestId: id, action: 'started' })), true);
    assert.equal(s.controller.messageFrom(s.peer, {}, s.lensState(true)), true);
    assert.equal(s.states.at(-1).active, false);
    assert.equal(s.controller.messageFrom(s.peer, s.channel, JSON.stringify({ type: 'stream_state' })), false);
    assert.equal(s.controller.messageFrom(s.peer, s.channel, 'not json'), false);
});

test('losing the connection turns lens 2 off and blocks requests', () => {
    const s = setup();
    s.controller.messageFrom(s.peer, s.channel, s.lensState(true));
    assert.equal(s.states.at(-1).active, true);
    s.peer.iceConnectionState = 'disconnected';
    s.controller.connection(s.peer);
    assert.equal(s.states.at(-1).active, false);
    assert.equal(s.states.at(-1).message, 'Connection lost');
    assert.equal(s.controller.start(), null);
    s.controller.reset();
    assert.equal(s.states.at(-1).available, false);
});

test('a channel that is not open cannot be used', () => {
    const s = setup({ open: false });
    s.controller.messageFrom(s.peer, s.channel, s.lensState(false));
    assert.equal(s.states.at(-1).ready, false);
    assert.equal(s.controller.start(), null);
});
