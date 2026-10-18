// Shared DOM stubs for running static/herald.js under node.
//
// herald.js is a set of IIFEs that register delegated listeners against the
// DOM. This stubs just enough of the DOM for the file to load and captures the
// listeners registered on document and document.body, so a harness can replay
// events through the real shipped code rather than a copy of its logic.

'use strict';

const fs = require('fs');

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

// load evaluates herald.js against the stubs and returns the listeners it
// registered, keyed by event type, merged across document and document.body
// (delegated listeners on either see the same bubbled events in a browser).
function load(scriptPath) {
    const listeners = {};
    function capture(type, fn) {
        (listeners[type] = listeners[type] || []).push(fn);
    }

    const body = stubElement();
    body.addEventListener = capture;

    const store = {};
    global.localStorage = {
        getItem(k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
        setItem(k, v) { store[k] = String(v); },
        removeItem(k) { delete store[k]; },
    };

    global.window = { addEventListener() {}, localStorage: global.localStorage };
    global.document = {
        addEventListener: capture,
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
    return listeners;
}

// fire replays one event through every captured listener of its type.
function fire(listeners, event) {
    (listeners[event.type] || []).forEach(fn => fn(event));
}

module.exports = { stubElement, load, fire };
