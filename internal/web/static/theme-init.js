// Apply the saved theme before the page paints, so a dark-theme reader does not
// see a white flash. A blocking script in the head, loaded from the site because
// the Content-Security-Policy refuses an inline one.
(function () {
    var t = localStorage.getItem('herald-theme');
    if (t) document.documentElement.setAttribute('data-theme', t);
})();
