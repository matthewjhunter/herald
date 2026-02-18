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
