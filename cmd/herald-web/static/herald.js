// Herald Web UI — keyboard shortcuts and scroll restore
(function() {
    'use strict';

    // Expose helper for clearing the reading pane (called from sidebar links)
    window.heraldClearReadingPane = function() {
        var pane = document.getElementById('reading-pane');
        if (pane) pane.innerHTML = '<div class="empty-state">Select an article to read</div>';
    };

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

    // Unsubscribe feed button — show when a specific feed is selected
    document.addEventListener('click', function(e) {
        var link = e.target.closest('a[data-feed-id]');
        var btn = document.getElementById('unsubscribe-feed-btn');
        if (!btn) return;
        if (link) {
            btn.dataset.feedId = link.dataset.feedId;
            btn.title = 'Unsubscribe from ' + link.dataset.feedTitle;
            btn.style.display = '';
        } else if (e.target.closest('#sidebar a:not([data-feed-id])')) {
            // "All Articles" or "Starred" — hide button
            btn.style.display = 'none';
            btn.dataset.feedId = '';
        }
    });

    document.addEventListener('click', function(e) {
        var btn = e.target.closest('#unsubscribe-feed-btn');
        if (!btn || !btn.dataset.feedId) return;
        var feedID = btn.dataset.feedId;
        if (!confirm('Unsubscribe from this feed?')) return;
        fetch('/feeds/' + feedID, {method: 'DELETE'})
            .then(function(res) {
                if (res.ok) {
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
    });

    // Save scroll position before navigation
    document.addEventListener('htmx:beforeRequest', function(e) {
        var list = document.getElementById('article-list');
        if (list) sessionStorage.setItem('herald-scroll', list.scrollTop);
    });

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
