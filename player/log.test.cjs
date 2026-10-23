// Run: node --test server/player/log.test.cjs
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const html = fs.readFileSync(__dirname + '/ws.html', 'utf8');
const source = html.slice(html.indexOf('        function log('), html.indexOf('        function sendCustomMessage('));

// Minimal DOM: text is kept as text, and any innerHTML write is recorded.
function element(tag) {
    const node = { tag, className: '', children: [], htmlWrites: 0 };
    Object.defineProperty(node, 'textContent', {
        get: () => node.children.map(child => typeof child === 'string' ? child : child.textContent).join(''),
        set: value => { node.children = [String(value)]; }
    });
    Object.defineProperty(node, 'innerHTML', { set: () => { node.htmlWrites++; } });
    node.append = (...items) => node.children.push(...items);
    node.appendChild = child => node.children.push(child);
    return node;
}

function setup() {
    const dataChannelLog = element('div');
    const context = vm.createContext({ Date, document: { createElement: element }, dataChannelLog, _autoScroll: false });
    vm.runInContext(source + '\nthis.log = log;', context);
    return { log: context.log, dataChannelLog };
}

test('camera text containing HTML is shown as text', () => {
    const s = setup();
    const payload = '<img src=x onerror="alert(1)">';
    s.log(`Notification from camera: ${payload}`, 'info');
    assert.equal(s.dataChannelLog.htmlWrites, 0);
    const [entry] = s.dataChannelLog.children;
    assert.equal(entry.className, 'log-entry ');
    assert.equal(entry.children[0].className, 'log-time');
    assert.ok(entry.children.slice(1).every(child => typeof child === 'string'), 'message is a text node');
    assert.ok(entry.textContent.endsWith(' Notification from camera: ' + payload));
});

test('log types keep their classes', () => {
    const s = setup();
    s.log('a', 'error');
    s.log('b', 'success');
    s.log('c', 'warn');
    assert.deepEqual(s.dataChannelLog.children.map(entry => entry.className),
        ['log-entry log-error', 'log-entry log-success', 'log-entry log-warn']);
});
