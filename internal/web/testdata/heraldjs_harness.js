// Node harness for exercising static/herald.js outside a browser.
//
// herald.js is a set of IIFEs that register delegated listeners against the
// DOM. This stubs just enough of the DOM for the file to load, captures the
// listeners registered on document.body, and then replays htmx:configRequest
// events through them -- so the tests drive the real shipped code rather than a
// copy of its logic.
//
// Usage: node heraldjs_harness.js <path-to-herald.js> <cases-json>
// where cases-json is [{"path":"/articles?feed_id=44","hideRead":false}, ...].
// It prints one JSON array of {"path":..., "parameters":{...}, "url":...},
// where url is the request URL htmx would build from the two.

'use strict';

const fs = require('fs');

const scriptPath = process.argv[2];
const cases = JSON.parse(process.argv[3]);

function stubElement() {
    return {
        addEventListener() {},
        removeEventListener() {},
        setAttribute() {},
        getAttribute() { return null; },
        hasAttribute() { return false; },
        matches() { return false; },
        closest() { return null; },
        querySelectorAll() { return []; },
        querySelector() { return null; },
        classList: { add() {}, remove() {}, contains() { return false; }, toggle() {} },
        style: {},
        textContent: '',
        hidden: false,
    };
}

const bodyListeners = {};
const body = stubElement();
body.addEventListener = function(type, fn) {
    (bodyListeners[type] = bodyListeners[type] || []).push(fn);
};

const store = {};
global.localStorage = {
    getItem(k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem(k, v) { store[k] = String(v); },
    removeItem(k) { delete store[k]; },
};

global.window = { addEventListener() {}, localStorage: global.localStorage };
global.document = {
    addEventListener() {},
    removeEventListener() {},
    body: body,
    getElementById() { return null; },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    createElement() { return stubElement(); },
    readyState: 'complete',
};
global.IntersectionObserver = function() { return { observe() {}, disconnect() {} }; };
global.MutationObserver = function() { return { observe() {}, disconnect() {} }; };
global.htmx = { ajax() {}, process() {}, trigger() {}, on() {} };
global.window.htmx = global.htmx;

// eslint-disable-next-line no-eval
eval(fs.readFileSync(scriptPath, 'utf8'));

// finalURL mirrors how htmx 2 assembles a GET: the configured path is used
// verbatim and the (filtered) parameters are appended to it.
function finalURL(path, parameters) {
    const keys = Object.keys(parameters);
    if (keys.length === 0) return path;
    const q = keys.map(k => encodeURIComponent(k) + '=' + encodeURIComponent(parameters[k])).join('&');
    return path + (path.indexOf('?') < 0 ? '?' : '&') + q;
}

const results = cases.map(c => {
    // The toggle stores 'false' when read articles should be shown; anything
    // else (including an unset key) means hide-read.
    if (c.hideRead) {
        global.localStorage.removeItem('herald-hide-read');
    } else {
        global.localStorage.setItem('herald-hide-read', 'false');
    }
    const detail = { path: c.path, parameters: Object.assign({}, c.parameters), verb: 'get' };
    const event = { detail: detail, type: 'htmx:configRequest', preventDefault() {} };
    (bodyListeners['htmx:configRequest'] || []).forEach(fn => fn(event));
    return { path: detail.path, parameters: detail.parameters, url: finalURL(detail.path, detail.parameters) };
});

process.stdout.write(JSON.stringify(results));
