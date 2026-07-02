# Analytics

Herald ships with **no analytics and no tracking of any kind**. Nothing is sent
anywhere by default -- not to the Herald project, not to any third party. This is
the correct setting for a private, self-hosted install and you never have to
touch it.

If you run a public-facing instance and want to know how many people find your
landing page, Herald can optionally load a single [Umami](https://umami.is/)
tracker script. Umami is open source, self-hostable, cookieless, and does not
collect personal data -- which is why it is the only integration Herald supports.
You point it at **your own** Umami server; Herald has no hosted analytics and no
default endpoint.

## What is (and is not) tracked

- **Only the public landing page** (`/`, shown to anonymous visitors) is ever
  instrumented. That is the acquisition surface -- "did anyone discover Herald" --
  and it contains no private data.
- **The authenticated reader is never tracked.** Once a visitor signs in, every
  page they see is served under Herald's strict Content-Security-Policy with no
  analytics script and no off-origin connections. Your feeds, article IDs, search
  queries, and read state never reach an analytics host. This boundary is
  structural: the tracker snippet lives only in the public page layout
  (`base_public.html`), and the CSP is only widened on public-page responses.

## Enabling it

Add a `[web.analytics]` section to your config. **Both** keys are required; if
either is missing or the URL is malformed, analytics stays off and Herald logs a
one-line warning at startup rather than failing to boot.

```toml
[web.analytics]
# Full URL of the Umami tracker script served by YOUR Umami instance.
umami_src  = "https://umami.example.com/script.js"
# The website UUID Umami assigns when you add a site (its data-website-id).
website_id = "00000000-0000-0000-0000-000000000000"
```

To get these values:

1. Stand up Umami (Docker image, `ghcr.io/umami-software/umami`) behind TLS.
2. In the Umami dashboard, **Settings -> Websites -> Add website**. Use your
   Herald landing page's public hostname as the domain.
3. Copy the **website ID** (a UUID) into `website_id`.
4. Your `umami_src` is your Umami base URL plus `/script.js`.

Restart `herald serve`. The landing page will now include, in its `<head>`:

```html
<script defer src="https://umami.example.com/script.js"
        data-website-id="00000000-0000-0000-0000-000000000000"></script>
```

To turn analytics back off, delete the `[web.analytics]` section (or blank either
key) and restart. There is no other state to clean up.

## Security model

Herald serves a deliberately strict Content-Security-Policy: `default-src
'self'`, no off-origin scripts, no off-origin connections. Loading an external
tracker would normally be blocked by that policy -- correctly.

When (and only when) analytics is enabled, Herald relaxes the CSP **for
public-page responses only**, and by the minimum needed:

- the tracker's origin (scheme + host, e.g. `https://umami.example.com` -- never
  the full URL, never a wildcard) is added to `script-src`, so the browser will
  load the script;
- the same origin is added to `connect-src`, so the script may POST pageview
  events back to your Umami server;
- nothing else changes, and the origin appears in no other directive.

The authenticated app's CSP is byte-for-byte identical whether or not analytics
is configured. The origin is parsed and validated once at startup, so a
malformed value can never inject into the header, and a bad setting degrades to
"analytics off" instead of taking the reader down.

## Why only Umami, and why client-side

- **Umami**, because it is privacy-respecting by design (no cookies, no
  cross-site identifiers, aggregate-only) and self-hostable, so you are not
  handing your visitors to a third-party ad-tech network to answer a simple
  question.
- **Client-side** (a script the browser runs), rather than server-side log
  parsing, because the goal is counting *human discovery*. A script that only
  runs in real browsers filters out the bots and crawlers that dominate raw
  request logs, and because it lives solely in the public page template it is
  inherently scoped to exactly the page you want measured -- no request-path
  filtering to configure and get wrong.

If you would rather do server-side tracking at your reverse proxy (for example
the `traefik-umami-feeder` plugin), that is entirely outside Herald: leave
`[web.analytics]` unset and Herald's CSP stays fully locked down.
