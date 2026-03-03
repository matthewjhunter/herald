# Fever API

Herald implements the [Fever API](https://feedafever.com/api), an open sync protocol originally designed for the Fever RSS reader. Most third-party RSS clients support it as a first-class sync backend, making it the easiest way to read your Herald feeds in a native app.

## Setup

1. In Herald, go to **Settings → Fever API**.
2. Enter any email and password — these are only used by your RSS client to authenticate. Herald stores only the MD5 hash; your actual credentials are never saved.
3. Click **Enable Fever API**.
4. Configure your RSS client (see below) using:
   - **Server URL:** the address shown on the settings page, e.g. `https://herald.example.com/fever/`
   - **Email / Password:** whatever you entered in step 2

> **Note:** The Fever email and password are separate from your Herald login. You can use anything you like; pick something you don't reuse elsewhere since it travels as an MD5 hash.

To rotate credentials, return to Settings, expand "Change credentials", and submit a new email/password. To disable Fever access entirely, click **Remove**.

---

## Compatible clients

### iOS / iPadOS

| App | Notes |
|-----|-------|
| [Reeder](https://reederapp.com) | Full Fever support. Add account → Fever → enter server URL and credentials. |
| [NetNewsWire](https://netnewswire.com) | Free and open source. Add account → Fever. |
| [Unread](https://www.goldenhillsoftware.com/unread/) | Fever supported under "Add Account". |
| [Fiery Feeds](https://cocoacake.net/apps/fiery/) | Fever supported under Sync Services. |
| [Lire](https://lireapp.com) | Fever supported under account settings. |

### macOS

| App | Notes |
|-----|-------|
| [Reeder](https://reederapp.com) | Same setup as iOS. |
| [NetNewsWire](https://netnewswire.com) | Free, open source, Fever sync built in. |
| [ReadKit](https://readkitapp.com) | Fever listed under "Add Account". |
| [News Explorer](https://betamagic.nl/products/newsexplorer.html) | Fever supported. |

### Android

| App | Notes |
|-----|-------|
| [FeedMe](https://play.google.com/store/apps/details?id=com.seazon.feedme) | Fever support under account settings. |
| [Palabre](https://play.google.com/store/apps/details?id=com.levelup.palabre) | Fever supported. |

### Desktop / cross-platform

| App | Notes |
|-----|-------|
| [Fluent Reader](https://hyliu.me/fluent-reader/) | Open source Electron app, Fever supported. |

---

## What syncs

The Fever protocol syncs:

- **Feed list** — your Herald subscriptions appear as feeds in the client
- **Articles** — full content and metadata, paginated
- **Read state** — marking articles read in the client marks them read in Herald, and vice versa
- **Starred / saved** — starring in the client sets the starred flag in Herald

Herald returns article clusters as Fever groups. Most clients display these as folders.

The `&links` endpoint returns Herald's AI-curated article clusters as Fever hot links. In clients like Reeder, these appear in the **Sparks** section. Each cluster becomes one link; the `temperature` field reflects the article's interest score (or cluster size if no score is available), and `item_ids` carries all related article IDs so the client can show full coverage of a topic.

---

## Security notes

- The Fever API uses HTTP Basic-style authentication (MD5 hash in the POST body). It is **not** secure over plain HTTP — always serve Herald over HTTPS.
- The API key is a one-way hash; Herald cannot recover your Fever email or password.
- Rotating credentials (or clicking Remove) immediately invalidates all client sessions.
- The `/fever/` endpoint does not use your Herald JWT cookie; it is separately authenticated and cannot be used to access other Herald API endpoints.
