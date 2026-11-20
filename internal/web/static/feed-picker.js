(function () {
    function esc(s) { return String(s).replace(/[&<>"]/g, function (c) { return ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c]; }); }

    function buildPicker(pick) {
        if (pick.dataset.fpInit) return;
        pick.dataset.fpInit = '1';
        var tagSel = pick.querySelector('.fp-tags');
        var feedSel = pick.querySelector('.fp-feeds');
        if (!feedSel) return;

        var feeds = Array.prototype.map.call(feedSel.options, function (o) {
            var tags = [];
            try { tags = JSON.parse(o.dataset.tags || '[]') || []; } catch (e) { tags = []; }
            return { id: o.value, title: o.dataset.title || o.text, tags: tags, opt: o };
        });
        var tagOpts = tagSel ? Array.prototype.slice.call(tagSel.options) : [];

        var ui = document.createElement('div');
        ui.className = 'fp-panes';
        ui.innerHTML =
            '<div class="fp-pane fp-avail"><h5>Available</h5>' +
            '<input type="search" class="fp-search" placeholder="filter feeds…"><div class="fp-scroll"></div></div>' +
            '<div class="fp-pane fp-incl"><h5>Included</h5><div class="fp-scroll fp-incl-body"></div></div>';
        pick.appendChild(ui);
        pick.classList.add('fp-on');

        var availBody = ui.querySelector('.fp-avail .fp-scroll');
        var inclBody = ui.querySelector('.fp-incl-body');
        var search = ui.querySelector('.fp-search');

        function tagSelected(name) {
            var o = tagOpts.find(function (t) { return t.value === name; });
            return o ? o.selected : false;
        }
        function setTag(name, on) {
            var o = tagOpts.find(function (t) { return t.value === name; });
            if (o) o.selected = on;
        }

        function renderAvail() {
            var q = (search.value || '').toLowerCase();
            var groups = {}; var order = [];
            tagOpts.forEach(function (t) { groups[t.value] = []; order.push(t.value); });
            var untagged = [];
            feeds.forEach(function (f) {
                if (q && f.title.toLowerCase().indexOf(q) === -1) return;
                if (!f.tags.length) { untagged.push(f); return; }
                f.tags.forEach(function (tg) { if (!(tg in groups)) { groups[tg] = []; order.push(tg); } groups[tg].push(f); });
            });
            var html = '';
            order.forEach(function (tg) {
                if (!groups[tg].length) return;
                html += '<details class="fp-group" open><summary>' + esc(tg) +
                    ' <button type="button" class="outline" data-follow="' + esc(tg) + '">' +
                    (tagSelected(tg) ? 'following ✓' : 'follow →') + '</button></summary>';
                groups[tg].forEach(function (f) {
                    html += '<div class="fp-row"><span>' + esc(f.title) + '</span>' +
                        '<button type="button" class="outline" data-add="' + esc(f.id) + '">' + (f.opt.selected ? '✓' : '+') + '</button></div>';
                });
                html += '</details>';
            });
            if (untagged.length) {
                html += '<details class="fp-group"><summary>Untagged</summary>';
                untagged.forEach(function (f) {
                    html += '<div class="fp-row"><span>' + esc(f.title) + '</span>' +
                        '<button type="button" class="outline" data-add="' + esc(f.id) + '">' + (f.opt.selected ? '✓' : '+') + '</button></div>';
                });
                html += '</details>';
            }
            availBody.innerHTML = html || '<p class="fp-included-empty">No feeds.</p>';
        }

        function renderIncl() {
            var html = '';
            tagOpts.filter(function (t) { return t.selected; }).forEach(function (t) {
                html += '<span class="fp-chip fp-tagchip">#' + esc(t.value) +
                    '<button type="button" data-unfollow="' + esc(t.value) + '">✕</button></span>';
            });
            feeds.filter(function (f) { return f.opt.selected; }).forEach(function (f) {
                html += '<span class="fp-chip">' + esc(f.title) +
                    '<button type="button" data-rm="' + esc(f.id) + '">✕</button></span>';
            });
            inclBody.innerHTML = html || '<p class="fp-included-empty">Nothing selected → all feeds.</p>';
        }

        function rerender() { renderAvail(); renderIncl(); }

        pick.addEventListener('click', function (e) {
            var b = e.target.closest('button'); if (!b || !pick.contains(b)) return;
            if (b.dataset.add != null) { var f = feeds.find(function (x) { return x.id === b.dataset.add; }); if (f) f.opt.selected = true; e.preventDefault(); rerender(); }
            else if (b.dataset.rm != null) { var f2 = feeds.find(function (x) { return x.id === b.dataset.rm; }); if (f2) f2.opt.selected = false; e.preventDefault(); rerender(); }
            else if (b.dataset.follow != null) { setTag(b.dataset.follow, true); e.preventDefault(); rerender(); }
            else if (b.dataset.unfollow != null) { setTag(b.dataset.unfollow, false); e.preventDefault(); rerender(); }
        });
        search.addEventListener('input', renderAvail);
        rerender();
    }

    function init(root) { (root || document).querySelectorAll('.feed-picker').forEach(buildPicker); }
    if (document.readyState !== 'loading') init(); else document.addEventListener('DOMContentLoaded', function () { init(); });
    document.body.addEventListener('htmx:afterSwap', function (e) { init(e.target); });
})();
