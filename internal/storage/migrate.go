package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// sanitizeStr strips invalid UTF-8 byte sequences. SQLite accepts arbitrary
// bytes; PostgreSQL requires valid UTF-8 and rejects rows that contain invalid
// sequences (e.g. article titles truncated at a byte boundary).
func sanitizeStr(s string) string {
	return strings.ToValidUTF8(s, "")
}

// MigrateStats reports how many rows were copied per table.
type MigrateStats struct {
	Feeds         int
	Users         int
	Articles      int
	ReadStates    int
	Subscriptions int
	Preferences   int
	Prompts       int
	Groups        int
	FilterRules   int
	FeverCreds    int
	Favicons      int
	Images        int
}

// MigrateStore copies all data from src to dst, remapping auto-generated IDs.
// dst should be empty; existing rows with conflicting keys are skipped.
//
// The full data model is preserved: feeds, users, articles, read states,
// subscriptions, preferences, prompts, groups, filter rules, fever credentials,
// favicons, and article images.
//
// Note: created_at timestamps on feeds, users, and groups are not preserved —
// they are set to the current time by the destination schema defaults.
func MigrateStore(ctx context.Context, src, dst Store) (*MigrateStats, error) {
	srcDB := tracedDBOf(src)
	dstDB := tracedDBOf(dst)

	feedMap := make(map[int64]int64)    // src feedID    → dst feedID
	userMap := make(map[int64]int64)    // src userID    → dst userID
	articleMap := make(map[int64]int64) // src articleID → dst articleID
	groupMap := make(map[int64]int64)   // src groupID   → dst groupID

	stats := &MigrateStats{}

	steps := []struct {
		name string
		fn   func() error
	}{
		{"feeds", func() error { return migrateFeeds(ctx, srcDB, dst, dstDB, feedMap, stats) }},
		{"users", func() error { return migrateUsers(ctx, srcDB, dst, userMap, stats) }},
		{"orphan_users", func() error { return migrateOrphanUsers(ctx, srcDB, dst, userMap) }},
		{"articles", func() error { return migrateArticles(ctx, srcDB, dst, dstDB, feedMap, articleMap, stats) }},
		{"subscriptions", func() error { return migrateUserFeeds(ctx, srcDB, dst, userMap, feedMap, stats) }},
		{"read_state", func() error { return migrateReadStates(ctx, srcDB, dstDB, userMap, articleMap, stats) }},
		{"user_preferences", func() error { return migrateUserPreferences(ctx, srcDB, dst, userMap, stats) }},
		{"user_prompts", func() error { return migrateUserPrompts(ctx, srcDB, dst, userMap, stats) }},
		{"article_authors", func() error { return migrateArticleAuthors(ctx, srcDB, dst, articleMap) }},
		{"article_categories", func() error { return migrateArticleCategories(ctx, srcDB, dst, articleMap) }},
		{"article_summaries", func() error { return migrateArticleSummaries(ctx, srcDB, dst, userMap, articleMap) }},
		{"article_groups", func() error { return migrateArticleGroups(ctx, srcDB, dst, userMap, articleMap, groupMap, stats) }},
		{"filter_rules", func() error { return migrateFilterRules(ctx, srcDB, dst, userMap, feedMap, stats) }},
		{"fever_credentials", func() error { return migrateFeverCredentials(ctx, srcDB, dst, userMap, stats) }},
		{"feed_favicons", func() error { return migrateFeedFavicons(ctx, srcDB, dst, feedMap, stats) }},
		{"article_images", func() error { return migrateArticleImages(ctx, srcDB, dst, articleMap, stats) }},
	}

	for _, step := range steps {
		if err := step.fn(); err != nil {
			return stats, fmt.Errorf("migrate %s: %w", step.name, err)
		}
		if err := ctx.Err(); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// tracedDBOf extracts the underlying *tracedDB from a Store.
// Panics if the concrete type is unknown — only SQLiteStore and PostgresStore
// are supported as migration sources/destinations.
func tracedDBOf(s Store) *tracedDB {
	switch v := s.(type) {
	case *SQLiteStore:
		return v.db
	case *PostgresStore:
		return v.db
	default:
		panic(fmt.Sprintf("MigrateStore: unsupported store type %T", s))
	}
}

// --- per-table helpers ---

// migrateOrphanUsers creates destination users for any user_id values referenced
// in the source's user_feeds / read_state / preferences / prompts tables that do
// not correspond to a row in the source's users table. This handles installations
// where a default user_id=1 was used directly without a users table entry.
func migrateOrphanUsers(ctx context.Context, src *tracedDB, dst Store, userMap map[int64]int64) error {
	tables := []string{"user_feeds", "read_state", "user_preferences", "user_prompts"}
	orphans := map[int64]bool{}
	for _, tbl := range tables {
		rows, err := src.QueryContext(ctx, "SELECT DISTINCT user_id FROM "+tbl)
		if err != nil {
			continue // table may not have rows
		}
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				if _, ok := userMap[id]; !ok {
					orphans[id] = true
				}
			}
		}
		rows.Close()
	}

	for srcID := range orphans {
		name := fmt.Sprintf("user_%d", srcID)
		dstID, err := dst.CreateUser(name)
		if err != nil {
			return fmt.Errorf("create synthetic user for id %d: %w", srcID, err)
		}
		userMap[srcID] = dstID
	}
	return nil
}

func migrateFeeds(ctx context.Context, src *tracedDB, dst Store, dstDB *tracedDB, feedMap map[int64]int64, stats *MigrateStats) error {
	rows, err := src.QueryContext(ctx, `
		SELECT id, url, title, COALESCE(description,''),
		       last_fetched, last_error, etag, last_modified,
		       enabled, consecutive_errors, next_fetch_at, status
		FROM feeds ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type feedRow struct {
		srcID             int64
		url, title, desc  string
		lastFetched       sql.NullTime
		lastError         sql.NullString
		etag              sql.NullString
		lastMod           sql.NullString
		enabled           bool
		consecutiveErrors int
		nextFetchAt       sql.NullTime
		status            string
	}

	var feeds []feedRow
	for rows.Next() {
		var f feedRow
		if err := rows.Scan(
			&f.srcID, &f.url, &f.title, &f.desc,
			&f.lastFetched, &f.lastError, &f.etag, &f.lastMod,
			&f.enabled, &f.consecutiveErrors, &f.nextFetchAt, &f.status,
		); err != nil {
			return fmt.Errorf("scan feed: %w", err)
		}
		feeds = append(feeds, f)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, f := range feeds {
		dstID, err := dst.AddFeed(f.url, sanitizeStr(f.title), sanitizeStr(f.desc))
		if err != nil {
			return fmt.Errorf("AddFeed %q: %w", f.url, err)
		}
		feedMap[f.srcID] = dstID

		// Restore fields not covered by AddFeed.
		var lastErrorVal *string
		if f.lastError.Valid {
			lastErrorVal = &f.lastError.String
		}
		var etagVal, lastModVal *string
		if f.etag.Valid {
			etagVal = &f.etag.String
		}
		if f.lastMod.Valid {
			lastModVal = &f.lastMod.String
		}
		if _, err := dstDB.ExecContext(ctx, `
			UPDATE feeds SET
			  last_error         = ?,
			  etag               = ?,
			  last_modified      = ?,
			  enabled            = ?,
			  consecutive_errors = ?,
			  status             = ?
			WHERE id = ?`,
			lastErrorVal, etagVal, lastModVal,
			f.enabled, f.consecutiveErrors, f.status,
			dstID,
		); err != nil {
			return fmt.Errorf("restore feed metadata %d: %w", dstID, err)
		}
		if f.lastFetched.Valid {
			if _, err := dstDB.ExecContext(ctx,
				"UPDATE feeds SET last_fetched = ? WHERE id = ?",
				f.lastFetched.Time, dstID,
			); err != nil {
				return fmt.Errorf("restore feed last_fetched %d: %w", dstID, err)
			}
		}
		if f.nextFetchAt.Valid {
			if _, err := dstDB.ExecContext(ctx,
				"UPDATE feeds SET next_fetch_at = ? WHERE id = ?",
				f.nextFetchAt.Time, dstID,
			); err != nil {
				return fmt.Errorf("restore feed next_fetch_at %d: %w", dstID, err)
			}
		}
		stats.Feeds++
	}
	return nil
}

func migrateUsers(ctx context.Context, src *tracedDB, dst Store, userMap map[int64]int64, stats *MigrateStats) error {
	rows, err := src.QueryContext(ctx,
		"SELECT id, name, oidc_sub, email FROM users ORDER BY id")
	if err != nil {
		return err
	}
	defer rows.Close()

	type userRow struct {
		srcID   int64
		name    string
		oidcSub sql.NullString
		email   sql.NullString
	}

	var users []userRow
	for rows.Next() {
		var u userRow
		if err := rows.Scan(&u.srcID, &u.name, &u.oidcSub, &u.email); err != nil {
			return fmt.Errorf("scan user: %w", err)
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, u := range users {
		var dstID int64
		if u.oidcSub.Valid {
			email := ""
			if u.email.Valid {
				email = u.email.String
			}
			created, err := dst.CreateUserWithOIDC(u.name, email, u.oidcSub.String)
			if err != nil {
				return fmt.Errorf("CreateUserWithOIDC %q: %w", u.name, err)
			}
			dstID = created.ID
		} else {
			id, err := dst.CreateUser(u.name)
			if err != nil {
				return fmt.Errorf("CreateUser %q: %w", u.name, err)
			}
			dstID = id
		}
		userMap[u.srcID] = dstID
		stats.Users++
	}
	return nil
}

func migrateArticles(ctx context.Context, src *tracedDB, dst Store, dstDB *tracedDB, feedMap, articleMap map[int64]int64, stats *MigrateStats) error {
	rows, err := src.QueryContext(ctx, `
		SELECT id, feed_id, guid, title, url,
		       COALESCE(content,''), COALESCE(summary,''), COALESCE(author,''),
		       published_date, fetched_date,
		       COALESCE(linked_url,''), COALESCE(linked_content,''),
		       full_text_fetched, images_cached
		FROM articles ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type articleRow struct {
		srcID            int64
		srcFeedID        int64
		guid, title, url string
		content, summary string
		author           string
		publishedDate    sql.NullTime
		fetchedDate      sql.NullTime
		linkedURL        string
		linkedContent    string
		fullTextFetched  bool
		imagesCached     bool
	}

	var articles []articleRow
	for rows.Next() {
		var a articleRow
		if err := rows.Scan(
			&a.srcID, &a.srcFeedID, &a.guid, &a.title, &a.url,
			&a.content, &a.summary, &a.author,
			&a.publishedDate, &a.fetchedDate,
			&a.linkedURL, &a.linkedContent,
			&a.fullTextFetched, &a.imagesCached,
		); err != nil {
			return fmt.Errorf("scan article: %w", err)
		}
		articles = append(articles, a)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, a := range articles {
		dstFeedID, ok := feedMap[a.srcFeedID]
		if !ok {
			return fmt.Errorf("article %d: no dst feed for src feed %d", a.srcID, a.srcFeedID)
		}

		art := &Article{
			FeedID:  dstFeedID,
			GUID:    a.guid,
			Title:   sanitizeStr(a.title),
			URL:     a.url,
			Content: sanitizeStr(a.content),
			Summary: sanitizeStr(a.summary),
			Author:  sanitizeStr(a.author),
		}
		if a.publishedDate.Valid {
			t := a.publishedDate.Time
			art.PublishedDate = &t
		}

		dstID, err := dst.AddArticle(art)
		if err != nil {
			return fmt.Errorf("AddArticle guid=%q: %w", a.guid, err)
		}
		if dstID == 0 {
			// Duplicate: look up the existing article ID.
			if err := dstDB.QueryRowContext(ctx,
				"SELECT id FROM articles WHERE feed_id = ? AND guid = ?",
				dstFeedID, a.guid,
			).Scan(&dstID); err != nil {
				return fmt.Errorf("lookup duplicate article guid=%q: %w", a.guid, err)
			}
		}
		articleMap[a.srcID] = dstID

		if a.linkedURL != "" {
			if err := dst.UpdateArticleLinkedContent(dstID, a.linkedURL, a.linkedContent); err != nil {
				return fmt.Errorf("UpdateArticleLinkedContent %d: %w", dstID, err)
			}
		}
		if a.fullTextFetched {
			if err := dst.MarkArticleFullTextFetched(dstID); err != nil {
				return fmt.Errorf("MarkArticleFullTextFetched %d: %w", dstID, err)
			}
		}
		if a.imagesCached {
			if err := dst.MarkArticleImagesCached(dstID); err != nil {
				return fmt.Errorf("MarkArticleImagesCached %d: %w", dstID, err)
			}
		}
		stats.Articles++
	}
	return nil
}

func migrateUserFeeds(ctx context.Context, src *tracedDB, dst Store, userMap, feedMap map[int64]int64, stats *MigrateStats) error {
	rows, err := src.QueryContext(ctx,
		"SELECT user_id, feed_id FROM user_feeds ORDER BY user_id, feed_id")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var srcUserID, srcFeedID int64
		if err := rows.Scan(&srcUserID, &srcFeedID); err != nil {
			return err
		}
		dstUserID, ok := userMap[srcUserID]
		if !ok {
			continue // user was skipped
		}
		dstFeedID, ok := feedMap[srcFeedID]
		if !ok {
			continue // feed was skipped
		}
		if err := dst.SubscribeUserToFeed(dstUserID, dstFeedID); err != nil {
			return fmt.Errorf("SubscribeUserToFeed: %w", err)
		}
		stats.Subscriptions++
	}
	return rows.Err()
}

func migrateReadStates(ctx context.Context, src, dst *tracedDB, userMap, articleMap map[int64]int64, stats *MigrateStats) error {
	rows, err := src.QueryContext(ctx, `
		SELECT user_id, article_id, read, starred,
		       interest_score, security_score, read_date, ai_scored
		FROM read_state ORDER BY user_id, article_id`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var srcUserID, srcArticleID int64
		var read, starred, aiScored bool
		var interestScore, securityScore sql.NullFloat64
		var readDate sql.NullTime

		if err := rows.Scan(
			&srcUserID, &srcArticleID,
			&read, &starred,
			&interestScore, &securityScore,
			&readDate, &aiScored,
		); err != nil {
			return fmt.Errorf("scan read_state: %w", err)
		}

		dstUserID, ok := userMap[srcUserID]
		if !ok {
			continue
		}
		dstArticleID, ok := articleMap[srcArticleID]
		if !ok {
			continue
		}

		var iScore, sScore *float64
		if interestScore.Valid {
			iScore = &interestScore.Float64
		}
		if securityScore.Valid {
			sScore = &securityScore.Float64
		}

		// Use a single direct INSERT that captures the full state atomically.
		// tracedDB.prepare() rewrites ? → $N for PostgreSQL.
		if _, err := dst.ExecContext(ctx, `
			INSERT INTO read_state
			  (user_id, article_id, read, starred, interest_score, security_score, read_date, ai_scored)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(user_id, article_id) DO NOTHING`,
			dstUserID, dstArticleID, read, starred, iScore, sScore,
			nullableTime(readDate), aiScored,
		); err != nil {
			return fmt.Errorf("insert read_state: %w", err)
		}
		stats.ReadStates++
	}
	return rows.Err()
}

func migrateUserPreferences(ctx context.Context, src *tracedDB, dst Store, userMap map[int64]int64, stats *MigrateStats) error {
	rows, err := src.QueryContext(ctx,
		"SELECT user_id, key, value FROM user_preferences ORDER BY user_id, key")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var srcUserID int64
		var key, value string
		if err := rows.Scan(&srcUserID, &key, &value); err != nil {
			return err
		}
		dstUserID, ok := userMap[srcUserID]
		if !ok {
			continue
		}
		if err := dst.SetUserPreference(dstUserID, key, value); err != nil {
			return fmt.Errorf("SetUserPreference: %w", err)
		}
		stats.Preferences++
	}
	return rows.Err()
}

func migrateUserPrompts(ctx context.Context, src *tracedDB, dst Store, userMap map[int64]int64, stats *MigrateStats) error {
	rows, err := src.QueryContext(ctx, `
		SELECT user_id, prompt_type, prompt_template, temperature, model
		FROM user_prompts ORDER BY user_id, prompt_type`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var srcUserID int64
		var promptType, template string
		var temp sql.NullFloat64
		var model sql.NullString
		if err := rows.Scan(&srcUserID, &promptType, &template, &temp, &model); err != nil {
			return err
		}
		dstUserID, ok := userMap[srcUserID]
		if !ok {
			continue
		}
		var tempPtr *float64
		if temp.Valid {
			tempPtr = &temp.Float64
		}
		var modelPtr *string
		if model.Valid {
			modelPtr = &model.String
		}
		if err := dst.SetUserPrompt(dstUserID, promptType, template, tempPtr, modelPtr); err != nil {
			return fmt.Errorf("SetUserPrompt: %w", err)
		}
		stats.Prompts++
	}
	return rows.Err()
}

func migrateArticleAuthors(ctx context.Context, src *tracedDB, dst Store, articleMap map[int64]int64) error {
	rows, err := src.QueryContext(ctx,
		"SELECT article_id, name, COALESCE(email,'') FROM article_authors ORDER BY article_id, name")
	if err != nil {
		return err
	}
	defer rows.Close()

	grouped := map[int64][]ArticleAuthor{}
	var order []int64
	for rows.Next() {
		var srcArticleID int64
		var name, email string
		if err := rows.Scan(&srcArticleID, &name, &email); err != nil {
			return err
		}
		if _, seen := grouped[srcArticleID]; !seen {
			order = append(order, srcArticleID)
		}
		grouped[srcArticleID] = append(grouped[srcArticleID], ArticleAuthor{Name: name, Email: email})
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, srcID := range order {
		dstID, ok := articleMap[srcID]
		if !ok {
			continue
		}
		if err := dst.StoreArticleAuthors(dstID, grouped[srcID]); err != nil {
			return fmt.Errorf("StoreArticleAuthors %d: %w", dstID, err)
		}
	}
	return nil
}

func migrateArticleCategories(ctx context.Context, src *tracedDB, dst Store, articleMap map[int64]int64) error {
	rows, err := src.QueryContext(ctx,
		"SELECT article_id, category FROM article_categories ORDER BY article_id, category")
	if err != nil {
		return err
	}
	defer rows.Close()

	grouped := map[int64][]string{}
	var order []int64
	for rows.Next() {
		var srcArticleID int64
		var category string
		if err := rows.Scan(&srcArticleID, &category); err != nil {
			return err
		}
		if _, seen := grouped[srcArticleID]; !seen {
			order = append(order, srcArticleID)
		}
		grouped[srcArticleID] = append(grouped[srcArticleID], category)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, srcID := range order {
		dstID, ok := articleMap[srcID]
		if !ok {
			continue
		}
		if err := dst.StoreArticleCategories(dstID, grouped[srcID]); err != nil {
			return fmt.Errorf("StoreArticleCategories %d: %w", dstID, err)
		}
	}
	return nil
}

func migrateArticleSummaries(ctx context.Context, src *tracedDB, dst Store, userMap, articleMap map[int64]int64) error {
	rows, err := src.QueryContext(ctx,
		"SELECT user_id, article_id, ai_summary FROM article_summaries ORDER BY user_id, article_id")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var srcUserID, srcArticleID int64
		var summary string
		if err := rows.Scan(&srcUserID, &srcArticleID, &summary); err != nil {
			return err
		}
		dstUserID, ok := userMap[srcUserID]
		if !ok {
			continue
		}
		dstArticleID, ok := articleMap[srcArticleID]
		if !ok {
			continue
		}
		if err := dst.UpdateArticleAISummary(dstUserID, dstArticleID, summary); err != nil {
			return fmt.Errorf("UpdateArticleAISummary: %w", err)
		}
	}
	return rows.Err()
}

func migrateArticleGroups(ctx context.Context, src *tracedDB, dst Store, userMap, articleMap, groupMap map[int64]int64, stats *MigrateStats) error {
	rows, err := src.QueryContext(ctx,
		"SELECT id, user_id, topic, embedding FROM article_groups ORDER BY id")
	if err != nil {
		return err
	}
	defer rows.Close()

	type groupRow struct {
		srcID     int64
		srcUserID int64
		topic     string
		embedding []byte
	}

	var groups []groupRow
	for rows.Next() {
		var g groupRow
		if err := rows.Scan(&g.srcID, &g.srcUserID, &g.topic, &g.embedding); err != nil {
			return err
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, g := range groups {
		dstUserID, ok := userMap[g.srcUserID]
		if !ok {
			continue
		}

		dstGroupID, err := dst.CreateArticleGroup(dstUserID, sanitizeStr(g.topic))
		if err != nil {
			return fmt.Errorf("CreateArticleGroup %q: %w", g.topic, err)
		}
		groupMap[g.srcID] = dstGroupID

		if len(g.embedding) > 0 {
			if err := dst.UpdateGroupEmbedding(dstGroupID, g.embedding); err != nil {
				return fmt.Errorf("UpdateGroupEmbedding %d: %w", dstGroupID, err)
			}
		}

		// Members
		mrows, err := src.QueryContext(ctx,
			"SELECT article_id FROM article_group_members WHERE group_id = ? ORDER BY added_at",
			g.srcID)
		if err != nil {
			return fmt.Errorf("query group members %d: %w", g.srcID, err)
		}
		for mrows.Next() {
			var srcArticleID int64
			if err := mrows.Scan(&srcArticleID); err != nil {
				mrows.Close()
				return err
			}
			dstArticleID, ok := articleMap[srcArticleID]
			if !ok {
				continue
			}
			if err := dst.AddArticleToGroup(dstGroupID, dstArticleID); err != nil {
				mrows.Close()
				return fmt.Errorf("AddArticleToGroup: %w", err)
			}
		}
		mrows.Close()
		if err := mrows.Err(); err != nil {
			return err
		}

		// Group summary
		var headline string
		var summary string
		var articleCount int
		var maxScore sql.NullFloat64
		err = src.QueryRowContext(ctx,
			"SELECT COALESCE(headline, ''), summary, article_count, max_interest_score FROM group_summaries WHERE group_id = ?",
			g.srcID,
		).Scan(&headline, &summary, &articleCount, &maxScore)
		if err == nil {
			var maxScorePtr *float64
			if maxScore.Valid {
				maxScorePtr = &maxScore.Float64
			}
			if err := dst.UpdateGroupSummary(dstGroupID, headline, summary, articleCount, maxScorePtr); err != nil {
				return fmt.Errorf("UpdateGroupSummary %d: %w", dstGroupID, err)
			}
		}

		stats.Groups++
	}
	return nil
}

func migrateFilterRules(ctx context.Context, src *tracedDB, dst Store, userMap, feedMap map[int64]int64, stats *MigrateStats) error {
	rows, err := src.QueryContext(ctx,
		"SELECT user_id, feed_id, axis, value, score FROM filter_rules ORDER BY id")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var srcUserID int64
		var srcFeedID sql.NullInt64
		var axis, value string
		var score int
		if err := rows.Scan(&srcUserID, &srcFeedID, &axis, &value, &score); err != nil {
			return err
		}
		dstUserID, ok := userMap[srcUserID]
		if !ok {
			continue
		}
		rule := &FilterRule{
			UserID: dstUserID,
			Axis:   axis,
			Value:  value,
			Score:  score,
		}
		if srcFeedID.Valid {
			dstFeedID, ok := feedMap[srcFeedID.Int64]
			if !ok {
				continue
			}
			rule.FeedID = &dstFeedID
		}
		if _, err := dst.AddFilterRule(rule); err != nil {
			return fmt.Errorf("AddFilterRule: %w", err)
		}
		stats.FilterRules++
	}
	return rows.Err()
}

func migrateFeverCredentials(ctx context.Context, src *tracedDB, dst Store, userMap map[int64]int64, stats *MigrateStats) error {
	rows, err := src.QueryContext(ctx,
		"SELECT user_id, api_key FROM fever_credentials ORDER BY user_id")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var srcUserID int64
		var apiKey string
		if err := rows.Scan(&srcUserID, &apiKey); err != nil {
			return err
		}
		dstUserID, ok := userMap[srcUserID]
		if !ok {
			continue
		}
		if err := dst.SetFeverCredential(dstUserID, apiKey); err != nil {
			return fmt.Errorf("SetFeverCredential: %w", err)
		}
		stats.FeverCreds++
	}
	return rows.Err()
}

func migrateFeedFavicons(ctx context.Context, src *tracedDB, dst Store, feedMap map[int64]int64, stats *MigrateStats) error {
	rows, err := src.QueryContext(ctx,
		"SELECT feed_id, data, mime_type FROM feed_favicons ORDER BY feed_id")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var srcFeedID int64
		var data []byte
		var mimeType string
		if err := rows.Scan(&srcFeedID, &data, &mimeType); err != nil {
			return err
		}
		dstFeedID, ok := feedMap[srcFeedID]
		if !ok {
			continue
		}
		if err := dst.StoreFeedFavicon(dstFeedID, data, mimeType); err != nil {
			return fmt.Errorf("StoreFeedFavicon: %w", err)
		}
		stats.Favicons++
	}
	return rows.Err()
}

func migrateArticleImages(ctx context.Context, src *tracedDB, dst Store, articleMap map[int64]int64, stats *MigrateStats) error {
	rows, err := src.QueryContext(ctx, `
		SELECT article_id, original_url, data, mime_type, width, height
		FROM article_images ORDER BY article_id, id`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var srcArticleID int64
		var originalURL, mimeType string
		var data []byte
		var width, height int
		if err := rows.Scan(&srcArticleID, &originalURL, &data, &mimeType, &width, &height); err != nil {
			return err
		}
		dstArticleID, ok := articleMap[srcArticleID]
		if !ok {
			continue
		}
		if _, err := dst.StoreArticleImage(dstArticleID, originalURL, data, mimeType, width, height); err != nil {
			return fmt.Errorf("StoreArticleImage: %w", err)
		}
		stats.Images++
	}
	return rows.Err()
}

// nullableTime converts a sql.NullTime to a *time.Time for use as a bind arg.
func nullableTime(nt sql.NullTime) interface{} {
	if nt.Valid {
		return nt.Time
	}
	return nil
}
