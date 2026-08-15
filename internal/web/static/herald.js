// Herald Web UI — keyboard shortcuts and scroll restore
(function() {
    'use strict';

    // Expose helper for clearing the reading pane (called from sidebar links)
    window.heraldClearReadingPane = function() {
        var pane = document.getElementById('reading-pane');
        if (pane) pane.innerHTML = '<div class="empty-state">Select an article to read</div>';
    };

    // Track the current sidebar selection so that feeds-changed refreshes
    // preserve the active highlight instead of resetting to "All Articles".
    window._heraldSidebarQuery = '';

    function heraldUpdateSidebarRefreshURL() {
        var sidebar = document.getElementById('sidebar');
        if (sidebar) {
            sidebar.setAttribute('hx-get', '/sidebar' + window._heraldSidebarQuery);
            htmx.process(sidebar); // re-scan so htmx picks up the new hx-get
        }
    }

    // Graceful auth-expiry handling. When the session cookie expires, any
    // request (including the periodic background sidebar refresh) gets a 401
    // with an HX-Redirect header. Left to htmx, that header triggers a full
    // page navigation to the login flow -- yanking the user off whatever
    // article they were reading. Instead we cancel htmx's response handling
    // and surface a non-destructive "reconnect" banner, preserving the
    // reading pane until the user chooses to re-authenticate.
    (function() {
        var banner = document.getElementById('reconnect-banner');
        var btn = document.getElementById('reconnect-btn');
        var loginURL = '/';

        document.body.addEventListener('htmx:beforeOnLoad', function(e) {
            var xhr = e.detail && e.detail.xhr;
            if (!xhr || xhr.status !== 401) return;
            // Cancelling beforeOnLoad stops htmx from swapping content AND from
            // honouring the HX-Redirect header, so the page stays put.
            e.preventDefault();
            loginURL = xhr.getResponseHeader('HX-Redirect') || loginURL;
            if (banner) banner.hidden = false;
        });

        if (btn) {
            btn.addEventListener('click', function() {
                window.location.href = loginURL;
            });
        }
    })();

    // Intercept sidebar link clicks to capture the current selection and clear
    // the reading pane. (The clear replaces the per-link hx-on:click handlers,
    // which the CSP -- no 'unsafe-eval' -- would otherwise block htmx from
    // compiling. See the htmx:afterRequest dispatcher below.)
    document.addEventListener('click', function(e) {
        var link = e.target.closest('#sidebar nav a[hx-get]');
        if (!link) return;
        var url = link.getAttribute('hx-get');
        var qIdx = url.indexOf('?');
        window._heraldSidebarQuery = qIdx >= 0 ? url.substring(qIdx) : '';
        heraldUpdateSidebarRefreshURL();
        window.heraldClearReadingPane();
    });

    // CSP-safe replacements for inline hx-on::after-request handlers. The app's
    // Content-Security-Policy omits 'unsafe-eval', which htmx requires to
    // compile hx-on attribute bodies via new Function(); those handlers fail
    // silently. This single delegated listener reproduces each one without eval.
    // Behaviors opt in via class or data-* attribute on the requesting element.
    document.body.addEventListener('htmx:afterRequest', function(e) {
        var d = e.detail || {};
        var elt = d.elt;
        if (!elt || !elt.matches) return;
        var ok = d.successful;

        // Article / summary row selection. Both row kinds carry .article-row;
        // only real article rows (data-article-id) get marked read and refresh
        // the sidebar counts.
        if (elt.classList.contains('article-row')) {
            document.querySelectorAll('.article-row').forEach(function(r) {
                r.classList.remove('active');
            });
            elt.classList.add('active');
            if (elt.hasAttribute('data-article-id')) {
                elt.classList.add('read');
                if (window.htmx) htmx.trigger(document.body, 'feeds-changed');
            }
        }

        // Everything below only runs after a successful request.
        if (!ok) return;

        // Refresh sidebar unread counts (e.g. the reading-pane read/unread toggle).
        if (elt.matches('[data-feeds-changed]') && window.htmx) {
            htmx.trigger(document.body, 'feeds-changed');
        }

        // Transient "Saved!" confirmation on a settings/admin form.
        if (elt.matches('[data-saved-feedback]')) {
            var btn = elt.querySelector('[data-save-btn]');
            if (btn) {
                btn.textContent = 'Saved!';
                setTimeout(function() { btn.textContent = 'Save'; }, 2000);
            }
        }

        // "Mark all as read" summary button: confirm and lock.
        if (elt.matches('[data-mark-read-feedback]')) {
            elt.textContent = 'Marked read';
            elt.disabled = true;
        }

        // OPML sync token regenerated: drop the new URL into its field and select it.
        var tokenSel = elt.getAttribute('data-opml-token-field');
        if (tokenSel && d.xhr) {
            var field = document.querySelector(tokenSel);
            if (field) {
                field.value = d.xhr.responseText;
                field.select();
            }
        }

        // Reset a form after a successful submit.
        if (elt.matches('[data-reset-on-success]') && typeof elt.reset === 'function') {
            elt.reset();
        }

        // Filter add-rule form: reset and clear the dependent value field.
        if (elt.matches('[data-filter-reset]')) {
            if (typeof elt.reset === 'function') elt.reset();
            var vf = document.getElementById('value-field');
            if (vf) {
                vf.innerHTML = '<select name="value" id="value-select" required>' +
                    '<option value="">— select feed and axis first —</option></select>';
            }
        }

        // Reload the page (run last; it tears everything down).
        if (elt.matches('[data-reload-on-success]')) {
            window.location.reload();
        }
    });

    // Theme toggle
    (function() {
        var btn = document.getElementById('theme-toggle');
        if (!btn) return;

        function currentTheme() {
            return document.documentElement.getAttribute('data-theme') || 'auto';
        }

        function updateBtn(theme) {
            btn.textContent = theme === 'dark' ? '☽' : '☀';
            btn.title = theme === 'dark' ? 'Switch to light mode' : 'Switch to dark mode';
        }

        updateBtn(currentTheme());

        btn.addEventListener('click', function() {
            var next = currentTheme() === 'dark' ? 'light' : 'dark';
            document.documentElement.setAttribute('data-theme', next);
            localStorage.setItem('herald-theme', next);
            updateBtn(next);
        });
    })();

    // Keyboard shortcuts
    document.addEventListener('keydown', function(e) {
        // Skip if user is typing in an input
        if (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA') return;

        var rows = document.querySelectorAll('.article-row');
        var active = document.querySelector('.article-row.active');
        var idx = active ? Array.from(rows).indexOf(active) : -1;

        switch (e.key) {
            case 'j': // Next article
                if (idx < rows.length - 1) rows[idx + 1].click();
                break;
            case 'k': // Previous article
                if (idx > 0) rows[idx - 1].click();
                break;
            case 'o': // Open original link
                var link = document.querySelector('.reading-pane a[data-original]');
                if (link) window.open(link.href, '_blank');
                break;
            case 's': // Toggle star
                var starBtn = document.querySelector('.reading-pane [data-star-toggle]');
                if (starBtn) starBtn.click();
                break;
        }
    });

    // Sidebar drag-to-resize
    (function() {
        var handle = document.getElementById('sidebar-resize-handle');
        var grid = document.getElementById('app-grid');
        if (!handle || !grid) return;

        // Restore saved width
        var saved = localStorage.getItem('herald-sidebar-width');
        if (saved) grid.style.setProperty('--sidebar-width', saved + 'px');

        var dragging = false;

        handle.addEventListener('mousedown', function(e) {
            dragging = true;
            handle.classList.add('dragging');
            e.preventDefault();
        });

        document.addEventListener('mousemove', function(e) {
            if (!dragging) return;
            var rect = grid.getBoundingClientRect();
            var width = Math.min(Math.max(e.clientX - rect.left, 150), 600);
            grid.style.setProperty('--sidebar-width', width + 'px');
        });

        document.addEventListener('mouseup', function() {
            if (!dragging) return;
            dragging = false;
            handle.classList.remove('dragging');
            var width = getComputedStyle(grid).getPropertyValue('--sidebar-width').trim();
            localStorage.setItem('herald-sidebar-width', parseInt(width));
        });
    })();

    // Vertical drag-to-resize (article list height)
    (function() {
        var handle = document.getElementById('vertical-resize-handle');
        var split = handle && handle.closest('.content-split');
        if (!handle || !split) return;

        var saved = localStorage.getItem('herald-list-height');
        if (saved) split.style.setProperty('--article-list-height', saved);

        var dragging = false;
        var lastPct = null;

        handle.addEventListener('mousedown', function(e) {
            dragging = true;
            handle.classList.add('dragging');
            e.preventDefault();
        });

        document.addEventListener('mousemove', function(e) {
            if (!dragging) return;
            var rect = split.getBoundingClientRect();
            var pct = (e.clientY - rect.top) / rect.height;
            lastPct = Math.min(Math.max(pct, 0.2), 0.75);
            split.style.setProperty('--article-list-height', (lastPct * 100) + '%');
        });

        document.addEventListener('mouseup', function() {
            if (!dragging) return;
            dragging = false;
            handle.classList.remove('dragging');
            if (lastPct !== null) {
                localStorage.setItem('herald-list-height', (lastPct * 100) + '%');
            }
        });
    })();

    // Context-sensitive footer buttons — show/hide based on sidebar selection
    document.addEventListener('click', function(e) {
        var feedLink = e.target.closest('a[data-feed-id]');
        var groupLink = e.target.closest('a[data-group-id]');
        var unsubBtn = document.getElementById('unsubscribe-feed-btn');
        var muteBtn = document.getElementById('mute-group-btn');
        var ungroupBtn = document.getElementById('ungroup-btn');

        if (feedLink) {
            // Feed selected — show unsubscribe, hide group buttons
            if (unsubBtn) {
                unsubBtn.dataset.feedId = feedLink.dataset.feedId;
                unsubBtn.title = 'Unsubscribe from ' + feedLink.dataset.feedTitle;
                unsubBtn.style.display = '';
            }
            if (muteBtn) muteBtn.style.display = 'none';
            if (ungroupBtn) ungroupBtn.style.display = 'none';
        } else if (groupLink) {
            // Group selected — show mute and ungroup, hide unsubscribe
            if (unsubBtn) { unsubBtn.style.display = 'none'; unsubBtn.dataset.feedId = ''; }
            if (muteBtn) {
                muteBtn.dataset.groupId = groupLink.dataset.groupId;
                muteBtn.title = 'Mute ' + groupLink.dataset.groupTitle;
                muteBtn.style.display = '';
            }
            if (ungroupBtn) {
                ungroupBtn.dataset.groupId = groupLink.dataset.groupId;
                ungroupBtn.title = 'Ungroup ' + groupLink.dataset.groupTitle;
                ungroupBtn.style.display = '';
            }
        } else if (e.target.closest('#sidebar a:not([data-feed-id]):not([data-group-id])')) {
            // "All Articles" or "Starred" — hide all action buttons
            if (unsubBtn) { unsubBtn.style.display = 'none'; unsubBtn.dataset.feedId = ''; }
            if (muteBtn) { muteBtn.style.display = 'none'; }
            if (ungroupBtn) { ungroupBtn.style.display = 'none'; }
        }
    });

    // Unsubscribe feed handler.
    //
    // The reason is optional and recorded as feedback (#252). It matters
    // because an unsubscribe is a tempting but usually wrong content signal:
    // dead feeds, format changes and volume cuts all land on the same button,
    // and mining them as topic negatives teaches the model to avoid subjects
    // because a server went away. "Just unsubscribe" is first and is the
    // default -- an unlabeled unsubscribe is honest, a guessed one is not.
    var UNSUB_REASONS = [
        {value: '', label: 'Just unsubscribe'},
        {value: 'broken', label: 'Feed is broken'},
        {value: 'volume', label: 'Too much volume'},
        {value: 'not_interested', label: 'Not interested'}
    ];

    function unsubscribeFeed(feedID, reason) {
        // Reason rides in the query string: net/http does not parse a request
        // body on DELETE, so a form-encoded body would be silently dropped.
        var path = '/feeds/' + feedID;
        if (reason) path += '?reason=' + encodeURIComponent(reason);
        fetch(path, {method: 'DELETE'}).then(function(res) {
            if (res.ok) window.location.href = '/';
        });
    }

    function closeUnsubMenu() {
        var open = document.getElementById('unsub-reason-menu');
        if (open) open.remove();
    }

    document.addEventListener('click', function(e) {
        var btn = e.target.closest('#unsubscribe-feed-btn');
        if (!btn || !btn.dataset.feedId) {
            if (!e.target.closest('#unsub-reason-menu')) closeUnsubMenu();
            return;
        }
        var feedID = btn.dataset.feedId;
        closeUnsubMenu();

        var menu = document.createElement('div');
        menu.id = 'unsub-reason-menu';
        menu.className = 'unsub-reason-menu';
        var label = document.createElement('small');
        label.className = 'secondary';
        label.textContent = 'Unsubscribe because:';
        menu.appendChild(label);

        UNSUB_REASONS.forEach(function(r) {
            var b = document.createElement('button');
            b.type = 'button';
            b.className = 'outline secondary';
            b.textContent = r.label;
            b.addEventListener('click', function() {
                closeUnsubMenu();
                unsubscribeFeed(feedID, r.value);
            });
            menu.appendChild(b);
        });

        var cancel = document.createElement('button');
        cancel.type = 'button';
        cancel.className = 'outline';
        cancel.textContent = 'Cancel';
        cancel.addEventListener('click', closeUnsubMenu);
        menu.appendChild(cancel);

        btn.parentNode.insertBefore(menu, btn.nextSibling);
    });

    // Mute group handler
    document.addEventListener('click', function(e) {
        var btn = e.target.closest('#mute-group-btn');
        if (!btn || !btn.dataset.groupId) return;
        fetch('/groups/' + btn.dataset.groupId + '/mute', {method: 'POST'})
            .then(function(res) {
                if (res.ok || res.status === 204) {
                    window.location.href = '/';
                }
            });
    });

    // Ungroup handler
    document.addEventListener('click', function(e) {
        var btn = e.target.closest('#ungroup-btn');
        if (!btn || !btn.dataset.groupId) return;
        if (!confirm('Ungroup these articles? They will return to their feeds.')) return;
        fetch('/groups/' + btn.dataset.groupId, {method: 'DELETE'})
            .then(function(res) {
                if (res.ok || res.status === 204) {
                    window.location.href = '/';
                }
            });
    });

    // Mark all as read
    document.addEventListener('click', function(e) {
        var btn = e.target.closest('.mark-all-read-btn');
        if (!btn) return;

        var ids = Array.from(document.querySelectorAll('#article-list .article-row[data-article-id]'))
            .map(function(el) { return el.dataset.articleId; })
            .filter(Boolean)
            .join(',');

        if (!ids) return;

        fetch('/articles/mark-all-read', {
            method: 'POST',
            headers: {'Content-Type': 'application/x-www-form-urlencoded'},
            body: 'ids=' + encodeURIComponent(ids)
        }).then(function(res) {
            if (res.ok || res.status === 204) {
                document.querySelectorAll('#article-list .article-row').forEach(function(el) {
                    el.classList.add('read');
                });
                htmx.trigger(document.body, 'feeds-changed');
            }
        });
    });

    // Restore scroll position after htmx swaps
    document.addEventListener('htmx:afterSwap', function(e) {
        if (e.detail.target.id === 'article-list') {
            var saved = sessionStorage.getItem('herald-scroll');
            if (saved) e.detail.target.scrollTop = parseInt(saved, 10);
        }
        if (e.detail.target.id === 'reading-pane') {
            e.detail.target.scrollTop = 0;
        }
    });

    // Save scroll position before navigation
    document.addEventListener('htmx:beforeRequest', function(e) {
        var list = document.getElementById('article-list');
        if (list) sessionStorage.setItem('herald-scroll', list.scrollTop);
    });

    // Hide-empty-feeds sidebar toggle
    (function() {
        var STORAGE_KEY = 'herald-hide-empty-feeds';
        var sidebar = document.getElementById('sidebar');

        function isHiding() {
            return localStorage.getItem(STORAGE_KEY) !== 'false';
        }

        function applyState() {
            var hiding = isHiding();
            if (sidebar) sidebar.classList.toggle('hide-empty-feeds', hiding);
            var btn = document.getElementById('hide-empty-feeds-btn');
            if (btn) btn.textContent = hiding ? 'Show all feeds' : 'Hide empty feeds';
        }

        document.addEventListener('click', function(e) {
            var btn = e.target.closest('#hide-empty-feeds-btn');
            if (!btn) return;
            localStorage.setItem(STORAGE_KEY, isHiding() ? 'false' : 'true');
            applyState();
        });

        // Re-apply after htmx re-renders sidebar innerHTML (button gets recreated)
        document.addEventListener('htmx:afterSwap', function(e) {
            if (e.detail.target.id === 'sidebar') applyState();
        });

        applyState();
    })();

    // Hide-read articles toggle.
    //
    // "Hide read" (the default) fetches only unread articles, matching the
    // server's default. "Show read" re-fetches the list with show_read=1 so
    // already-read articles -- including ones read in a previous session --
    // come back, rendered faded. The choice is authoritative for every request
    // that loads the article list (initial load, sidebar navigation, infinite
    // scroll) via the htmx:configRequest hook below, so it survives navigation.
    (function() {
        var STORAGE_KEY = 'herald-hide-read';
        var btn = document.getElementById('hide-read-btn');

        function isHiding() {
            return localStorage.getItem(STORAGE_KEY) !== 'false';
        }

        // Reflect the current mode in the button label. Read articles are not
        // hidden in-session -- a clicked article stays, dimmed, until the list
        // is refetched (configRequest below drops show_read in hide-read mode,
        // so the server-side unread filter removes it on the next fetch).
        function applyState() {
            if (btn) btn.textContent = isHiding() ? 'Show read' : 'Hide read';
        }

        // Make the toggle authoritative for every article-list request,
        // regardless of the URL htmx started from.
        //
        // htmx reports the whole request URL as detail.path -- query string
        // included -- so this has to compare the path alone. Matching the raw
        // detail.path against '/articles' silently skipped every feed, group
        // and infinite-scroll request, which is how "Show read" on a fully-read
        // feed came back empty. For the same reason show_read is rewritten in
        // the URL as well as in the parameters: dropping it from parameters
        // alone leaves a show_read=1 the URL already carried in place.
        document.body.addEventListener('htmx:configRequest', function(e) {
            var path = e.detail.path || '';
            var qIdx = path.indexOf('?');
            var base = qIdx >= 0 ? path.substring(0, qIdx) : path;
            if (base !== '/articles') return;

            var params = new URLSearchParams(qIdx >= 0 ? path.substring(qIdx + 1) : '');
            params.delete('show_read');
            var query = params.toString();
            e.detail.path = query ? base + '?' + query : base;

            if (isHiding()) {
                delete e.detail.parameters['show_read'];
            } else {
                e.detail.parameters['show_read'] = '1';
            }
        });

        if (btn) {
            btn.addEventListener('click', function() {
                localStorage.setItem(STORAGE_KEY, isHiding() ? 'false' : 'true');
                applyState();
                // Re-fetch the current view; configRequest adds/removes
                // show_read, and the OOB sidebar refreshes in the same swap.
                if (window.htmx) {
                    var url = '/articles' + (window._heraldSidebarQuery || '');
                    htmx.ajax('GET', url, { target: '#article-list', swap: 'innerHTML' });
                }
            });
        }

        applyState();
    })();

    // Sortable tables
    (function() {
        function cellValue(row, col) {
            var cell = row.cells[col];
            return cell ? cell.textContent.trim() : '';
        }

        function sortTable(table, col, asc) {
            var tbody = table.tBodies[0];
            var rows = Array.from(tbody.rows);
            rows.sort(function(a, b) {
                var av = cellValue(a, col);
                var bv = cellValue(b, col);
                // Numeric if both look numeric
                var an = parseFloat(av), bn = parseFloat(bv);
                if (!isNaN(an) && !isNaN(bn)) return asc ? an - bn : bn - an;
                // Empty strings sort last
                if (av === '' && bv !== '') return 1;
                if (bv === '' && av !== '') return -1;
                return asc ? av.localeCompare(bv) : bv.localeCompare(av);
            });
            rows.forEach(function(r) { tbody.appendChild(r); });
        }

        document.addEventListener('click', function(e) {
            var th = e.target.closest('th.sortable');
            if (!th) return;
            var table = th.closest('table');
            if (!table) return;
            var col = parseInt(th.dataset.col, 10);
            var asc = !th.classList.contains('sort-asc');
            // Reset all headers
            Array.from(table.querySelectorAll('th.sortable')).forEach(function(h) {
                h.classList.remove('sort-asc', 'sort-desc');
            });
            th.classList.add(asc ? 'sort-asc' : 'sort-desc');
            sortTable(table, col, asc);
        });
    })();
})();
