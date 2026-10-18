// Node harness for the AI summary "prompt" link toggle in static/herald.js.
//
// Replays a sequence of steps against one link and its target:
//   "click"  -- htmx is about to issue the link's request (htmx:beforeRequest);
//               reports whether herald.js cancelled it and the target's hidden
//               state afterwards.
//   "load"   -- the request completed and the prompt was swapped into the
//               target (htmx:afterSwap).
//
// Usage: node heraldjs_prompt_toggle_harness.js <path-to-herald.js> <case-json>
// where case-json is {"toggle": true|false, "steps": ["click","load","click"]}.
// toggle=false drives a link WITHOUT the data-summary-prompt-toggle marker, to
// show other htmx links are left alone. Prints one JSON array with an entry per
// step: {"step":..., "prevented":bool, "hidden":bool}.

'use strict';

const { stubElement, load, fire } = require('./heraldjs_stubs.js');

const scriptPath = process.argv[2];
const c = JSON.parse(process.argv[3]);
const listeners = load(scriptPath);

const link = stubElement();
link.hasAttribute = function(name) { return c.toggle && name === 'data-summary-prompt-toggle'; };
const target = stubElement();
target.childNodes = [];
target.classList.contains = function(cls) { return cls === 'ai-summary-prompt'; };

const results = c.steps.map(step => {
    let prevented = false;
    if (step === 'load') {
        target.childNodes = [stubElement()];
        fire(listeners, { type: 'htmx:afterSwap', detail: { elt: link, target: target }, preventDefault() {} });
    } else {
        fire(listeners, {
            type: 'htmx:beforeRequest',
            detail: { elt: link, target: target },
            preventDefault() { prevented = true; },
        });
    }
    return { step: step, prevented: prevented, hidden: !!target.hidden };
});

process.stdout.write(JSON.stringify(results));
