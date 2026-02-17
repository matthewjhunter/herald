# Changelog

All notable changes to FeedReader will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.0.0] - 2026-02-17

### Added

#### Core Features
- RSS 2.0 and Atom 1.0 feed parsing using gofeed library
- OPML import with support for nested folder structures
- SQLite database with read state tracking
- Command-line interface with Cobra framework
- Comprehensive test suite for storage layer

#### AI Integration
- Security screening using Gemma 2 model via Ollama
- Content curation using Llama 3.2 model via Ollama
- Two-model architecture for separation of security and curation
- Interest scoring system (0-10 scale)
- Security scoring system (0-10 scale)
- Configurable thresholds for both scores

#### Majordomo Integration
- Voice notifications for high-interest articles
- Chat command integration
- Graceful fallback when Majordomo unavailable

#### CLI Commands
- `init-config` - Create default configuration file
- `import <opml-file>` - Import feeds from OPML
- `fetch` - Fetch and process feeds with AI
- `list [--limit N]` - List unread articles
- `read <article-id>` - Mark article as read

#### Configuration
- YAML-based configuration system
- User preferences for keywords and sources
- Configurable Ollama models and endpoints
- Majordomo integration settings
- Threshold customization

#### Database Schema
- `feeds` table - Feed subscriptions
- `articles` table - Article storage with deduplication
- `read_state` table - Read status and AI scores
- `user_preferences` table - Future multi-user support

#### Documentation
- Comprehensive README with architecture overview
- Installation guide (INSTALL.md)
- Usage guide with examples (USAGE.md)
- Example configuration file
- Makefile for common tasks
- Inline code documentation

### Security
- Prompt injection detection via Gemma 2
- Malicious content screening
- Conservative security defaults (threshold: 7.0/10)
- Safe failure modes (rejects suspicious content)

### Performance
- Parallel article processing
- Efficient database queries with indexes
- Deduplication at database level (GUID-based)
- Optional cron-based automation

### Future Enhancements (Planned)

#### Phase 2
- Multi-user support with per-user read states
- Web interface for article browsing
- Full-text article extraction from web pages
- Learning mechanism based on reading patterns
- Explicit feedback system (like/dislike)
- Advanced analytics dashboard

#### Phase 3
- Mobile app integration
- Browser extension
- API for third-party integrations
- Advanced filtering and categorization
- Collaborative filtering recommendations
- Feed discovery and suggestions

## [0.1.0] - Development

### Initial Development
- Project scaffolding and Go module initialization
- Database schema design
- OPML parser implementation
- Feed fetcher core logic
- Basic CLI structure

---

## Version Numbering

- **Major version** (X.0.0): Breaking changes or major feature additions
- **Minor version** (0.X.0): New features, backward compatible
- **Patch version** (0.0.X): Bug fixes, minor improvements
