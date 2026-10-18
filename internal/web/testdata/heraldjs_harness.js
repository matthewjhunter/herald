// Node harness for the "Show read" toggle in static/herald.js.
//
// Replays htmx:configRequest events through the listeners herald.js registers
// (see heraldjs_stubs.js), so the tests drive the real shipped code.
//
// Usage: node heraldjs_harness.js <path-to-herald.js> <cases-json>
// where cases-json is [{"path":"/articles?feed_id=44","hideRead":false}, ...].
// It prints one JSON array of {"path":..., "parameters":{...}, "url":...},
// where url is the request URL htmx would build from the two.

'use strict';

const { load, fire } = require('./heraldjs_stubs.js');

const scriptPath = process.argv[2];
const cases = JSON.parse(process.argv[3]);
const listeners = load(scriptPath);

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
    fire(listeners, { detail: detail, type: 'htmx:configRequest', preventDefault() {} });
    return { path: detail.path, parameters: detail.parameters, url: finalURL(detail.path, detail.parameters) };
});

process.stdout.write(JSON.stringify(results));
