package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// handleFever is the single Fever API endpoint.
// Both GET and POST are accepted; authentication is via the api_key form value
// (MD5 of email:password), never via the JWT cookie.
//
// See https://feedafever.com/api for the full Fever API specification.
func (h *handlers) handleFever(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeFeverJSON(w, feverUnauth())
		return
	}

	apiKey := r.FormValue("api_key")
	if apiKey == "" {
		writeFeverJSON(w, feverUnauth())
		return
	}

	user, err := h.engine.GetUserByFeverAPIKey(apiKey)
	if err != nil {
		writeFeverJSON(w, feverUnauth())
		return
	}

	resp := map[string]any{
		"api_version":            3,
		"auth":                   1,
		"last_refreshed_on_time": time.Now().Unix(),
	}

	// Mark (write) operations first so subsequent reads reflect the change.
	if mark := r.FormValue("mark"); mark != "" {
		h.feverMark(user.ID, r)
	}

	// &feeds returns the subscription list plus feeds_groups.
	if _, ok := r.Form["feeds"]; ok {
		h.feverAddFeeds(user.ID, resp)
	}

	// &groups returns folder names; herald uses article groups, not feed folders,
	// so we return an empty array. feeds_groups is included for spec compliance.
	if _, ok := r.Form["groups"]; ok {
		resp["groups"] = []any{}
		if _, already := resp["feeds_groups"]; !already {
			h.feverAddFeedsGroups(user.ID, resp)
		}
	}

	if _, ok := r.Form["items"]; ok {
		h.feverAddItems(user.ID, r, resp)
	}

	if _, ok := r.Form["unread_item_ids"]; ok {
		h.feverAddUnreadIDs(user.ID, resp)
	}

	if _, ok := r.Form["saved_item_ids"]; ok {
		h.feverAddSavedIDs(user.ID, resp)
	}

	// &favicons returns base64-encoded feed icons.
	if _, ok := r.Form["favicons"]; ok {
		h.feverAddFavicons(resp)
	}
	// &links returns article clusters as Fever hot links.
	if _, ok := r.Form["links"]; ok {
		h.feverAddLinks(user.ID, resp)
	}

	writeFeverJSON(w, resp)
}

// feverUnauth returns the minimal unauthenticated Fever response.
func feverUnauth() map[string]any {
	return map[string]any{
		"api_version":            3,
		"auth":                   0,
		"last_refreshed_on_time": time.Now().Unix(),
	}
}

// feverMark processes a mark= write operation from the Fever API.
func (h *handlers) feverMark(userID int64, r *http.Request) {
	entity := r.FormValue("mark") // "item", "feed", "group"
	as := r.FormValue("as")       // "read", "unread", "saved", "unsaved"
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	before, _ := strconv.ParseInt(r.FormValue("before"), 10, 64)

	if id == 0 && entity != "group" {
		return
	}

	switch entity {
	case "item":
		var err error
		switch as {
		case "read":
			err = h.engine.MarkArticleRead(userID, id)
		case "unread":
			err = h.engine.MarkArticleUnread(userID, id)
		case "saved":
			err = h.engine.StarArticle(userID, id, true)
		case "unsaved":
			err = h.engine.StarArticle(userID, id, false)
		}
		if err != nil {
			log.Printf("fever: mark item %d as=%q user=%d: %v", id, as, userID, err)
		}
	case "feed":
		if as == "read" {
			if err := h.engine.FeverMarkFeedRead(userID, id, before); err != nil {
				log.Printf("fever: mark feed %d read user=%d: %v", id, userID, err)
			}
		}
	case "group":
		if as == "read" {
			var err error
			if id == 0 {
				// Group 0 means "all items".
				err = h.engine.FeverMarkAllRead(userID, before)
			} else {
				err = h.engine.FeverMarkGroupRead(userID, id, before)
			}
			if err != nil {
				log.Printf("fever: mark group %d read user=%d: %v", id, userID, err)
			}
		}
	}
}

// feverFeed is the Fever wire format for a single feed.
type feverFeed struct {
	ID                int64  `json:"id"`
	FaviconID         int64  `json:"favicon_id"`
	Title             string `json:"title"`
	URL               string `json:"url"`
	SiteURL           string `json:"site_url"`
	IsSpark           int    `json:"is_spark"`
	LastUpdatedOnTime int64  `json:"last_updated_on_time"`
}

func (h *handlers) feverAddFeeds(userID int64, resp map[string]any) {
	feeds, err := h.engine.GetFeverFeeds(userID)
	if err != nil {
		resp["feeds"] = []any{}
		resp["feeds_groups"] = []any{}
		return
	}

	// Build a set of feed IDs that have a cached favicon for fast lookup.
	faviconFeeds := make(map[int64]bool)
	if favicons, err := h.engine.GetAllFeedFavicons(); err == nil {
		for _, fav := range favicons {
			faviconFeeds[fav.FeedID] = true
		}
	}

	ff := make([]feverFeed, len(feeds))
	for i, f := range feeds {
		var lastUpdated int64
		if f.LastFetched != nil {
			lastUpdated = f.LastFetched.Unix()
		}
		var faviconID int64
		if faviconFeeds[f.ID] {
			faviconID = f.ID // favicon_id == feed_id in herald
		}
		ff[i] = feverFeed{
			ID:                f.ID,
			FaviconID:         faviconID,
			Title:             f.Title,
			URL:               f.URL,
			SiteURL:           f.URL, // no separate site URL stored
			LastUpdatedOnTime: lastUpdated,
		}
	}
	resp["feeds"] = ff
	h.feverAddFeedsGroups(userID, resp)
}

// feverFeedsGroup maps a group_id to the comma-separated feed IDs it contains.
type feverFeedsGroup struct {
	GroupID int64  `json:"group_id"`
	FeedIDs string `json:"feed_ids"`
}

func (h *handlers) feverAddFeedsGroups(userID int64, resp map[string]any) {
	memberships, err := h.engine.GetFeedGroupMemberships(userID)
	if err != nil || len(memberships) == 0 {
		resp["feeds_groups"] = []any{}
		return
	}

	fgs := make([]feverFeedsGroup, 0, len(memberships))
	for groupID, feedIDs := range memberships {
		parts := make([]string, len(feedIDs))
		for i, fid := range feedIDs {
			parts[i] = strconv.FormatInt(fid, 10)
		}
		fgs = append(fgs, feverFeedsGroup{
			GroupID: groupID,
			FeedIDs: strings.Join(parts, ","),
		})
	}
	resp["feeds_groups"] = fgs
}

// feverItem is the Fever wire format for a single article.
type feverItem struct {
	ID            int64  `json:"id"`
	FeedID        int64  `json:"feed_id"`
	Title         string `json:"title"`
	Author        string `json:"author"`
	HTML          string `json:"html"`
	URL           string `json:"url"`
	IsSaved       int    `json:"is_saved"`
	IsRead        int    `json:"is_read"`
	CreatedOnTime int64  `json:"created_on_time"`
}

func (h *handlers) feverAddItems(userID int64, r *http.Request, resp map[string]any) {
	sinceID, _ := strconv.ParseInt(r.FormValue("since_id"), 10, 64)
	maxID, _ := strconv.ParseInt(r.FormValue("max_id"), 10, 64)

	var withIDs []int64
	if raw := r.FormValue("with_ids"); raw != "" {
		for _, s := range strings.Split(raw, ",") {
			if id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
				withIDs = append(withIDs, id)
			}
		}
		if len(withIDs) > 100 {
			withIDs = withIDs[:100]
		}
	}

	rows, err := h.engine.GetFeverItems(userID, sinceID, maxID, withIDs, 50)
	if err != nil {
		resp["items"] = []any{}
		resp["total_items"] = 0
		return
	}

	total, _ := h.engine.GetFeverItemCount(userID)

	items := make([]feverItem, len(rows))
	for i, row := range rows {
		var ts int64
		if row.PublishedDate != nil {
			ts = row.PublishedDate.Unix()
		} else {
			ts = row.FetchedDate.Unix()
		}

		isRead := 0
		if row.IsRead {
			isRead = 1
		}
		isSaved := 0
		if row.IsStarred {
			isSaved = 1
		}

		html := row.Content
		if html == "" {
			html = row.Summary
		}

		items[i] = feverItem{
			ID:            row.ID,
			FeedID:        row.FeedID,
			Title:         row.Title,
			Author:        row.Author,
			HTML:          html,
			URL:           row.URL,
			IsSaved:       isSaved,
			IsRead:        isRead,
			CreatedOnTime: ts,
		}
	}
	resp["items"] = items
	resp["total_items"] = total
}

func (h *handlers) feverAddUnreadIDs(userID int64, resp map[string]any) {
	ids, err := h.engine.GetUnreadArticleIDsForUser(userID)
	if err != nil || len(ids) == 0 {
		resp["unread_item_ids"] = ""
		return
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	resp["unread_item_ids"] = strings.Join(parts, ",")
}

func (h *handlers) feverAddSavedIDs(userID int64, resp map[string]any) {
	ids, err := h.engine.GetStarredArticleIDsForUser(userID)
	if err != nil || len(ids) == 0 {
		resp["saved_item_ids"] = ""
		return
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	resp["saved_item_ids"] = strings.Join(parts, ",")
}

// feverLink is the Fever wire format for a single hot link (&links endpoint).
type feverLink struct {
	ID          int64  `json:"id"`
	FeedID      int64  `json:"feed_id"`
	ItemID      int64  `json:"item_id"`
	Temperature int    `json:"temperature"`
	IsItem      int    `json:"is_item"`
	IsLocal     int    `json:"is_local"`
	IsSaved     int    `json:"is_saved"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	ItemIDs     string `json:"item_ids"`
}

func (h *handlers) feverAddLinks(userID int64, resp map[string]any) {
	groups, err := h.engine.GetFeverLinks(userID)
	if err != nil {
		resp["links"] = []any{}
		return
	}

	links := make([]feverLink, len(groups))
	for i, g := range groups {
		links[i] = feverLink{
			ID:          g.GroupID,
			FeedID:      g.FeedID,
			ItemID:      g.ItemID,
			Temperature: g.Temperature,
			IsItem:      1,
			IsLocal:     1,
			IsSaved:     g.IsSaved,
			Title:       g.Title,
			URL:         g.URL,
			ItemIDs:     g.ItemIDs,
		}
	}
	resp["links"] = links
}

// feverFavicon is the Fever wire format for a favicon (&favicons endpoint).
type feverFavicon struct {
	ID   int64  `json:"id"`
	Data string `json:"data"` // "mime_type;base64,<base64-encoded-data>"
}

func (h *handlers) feverAddFavicons(resp map[string]any) {
	favicons, err := h.engine.GetAllFeedFavicons()
	if err != nil || len(favicons) == 0 {
		resp["favicons"] = []any{}
		return
	}
	ff := make([]feverFavicon, len(favicons))
	for i, f := range favicons {
		ff[i] = feverFavicon{
			ID:   f.FeedID,
			Data: fmt.Sprintf("%s;base64,%s", f.MimeType, base64.StdEncoding.EncodeToString(f.Data)),
		}
	}
	resp["favicons"] = ff
}

func writeFeverJSON(w http.ResponseWriter, data map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data)
}
