// Herald Web UI — keyboard shortcuts and scroll restore
(function() {
    'use strict';

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
