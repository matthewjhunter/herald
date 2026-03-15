package feeds

// Feed reader user-agent for RSS/Atom fetches. Honestly identifies as
// a feed reader -- this is legitimate and expected by feed publishers.
const FeedUserAgent = "Herald/1.0 (+https://github.com/matthewjhunter/herald)"

// Browser-like user-agent for page fetches (fulltext extraction,
// favicon discovery, image caching). These are normal page requests
// on behalf of a user, not AI scraping.
const PageUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Herald/1.0"
