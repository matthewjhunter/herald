// Herald Web UI — keyboard shortcuts and scroll restore
(function() {
    'use strict';

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

    // Mark all as read
    document.addEventListener('click', function(e) {
        var btn = e.target.closest('.mark-all-read-btn');
        if (!btn) return;

        var userID = btn.dataset.userId;
        var ids = Array.from(document.querySelectorAll('#article-list .article-row[data-article-id]'))
            .map(function(el) { return el.dataset.articleId; })
            .filter(Boolean)
            .join(',');

        if (!ids) return;

        fetch('/u/' + userID + '/articles/mark-all-read', {
            method: 'POST',
            headers: {'Content-Type': 'application/x-www-form-urlencoded'},
            body: 'ids=' + encodeURIComponent(ids)
        }).then(function(res) {
            if (res.ok || res.status === 204) {
                document.querySelectorAll('#article-list .article-row').forEach(function(el) {
                    el.classList.add('read');
                });
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
})();
