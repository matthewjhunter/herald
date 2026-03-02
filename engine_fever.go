package herald

import (
	"crypto/md5"
	"database/sql"
	"errors"
	"fmt"

	"github.com/matthewjhunter/herald/internal/storage"
)

// SetFeverCredential stores the Fever API key derived from MD5(email:password).
func (e *Engine) SetFeverCredential(userID int64, email, password string) error {
	sum := md5.Sum([]byte(email + ":" + password))
	apiKey := fmt.Sprintf("%x", sum)
	return e.store.SetFeverCredential(userID, apiKey)
}

// HasFeverCredential reports whether the user has a Fever API key configured.
func (e *Engine) HasFeverCredential(userID int64) (bool, error) {
	_, err := e.store.GetFeverAPIKey(userID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// DeleteFeverCredential removes the Fever API credential for a user.
func (e *Engine) DeleteFeverCredential(userID int64) error {
	return e.store.DeleteFeverCredential(userID)
}

// GetUserByFeverAPIKey returns the user matching the Fever API key.
func (e *Engine) GetUserByFeverAPIKey(apiKey string) (*storage.User, error) {
	return e.store.GetUserByFeverAPIKey(apiKey)
}

// GetFeverFeeds returns the user's subscribed feeds.
func (e *Engine) GetFeverFeeds(userID int64) ([]storage.Feed, error) {
	return e.store.GetUserFeeds(userID)
}

// GetFeverItems returns articles for the Fever items endpoint.
func (e *Engine) GetFeverItems(userID int64, sinceID, maxID int64, withIDs []int64, limit int) ([]storage.FeverItemRow, error) {
	return e.store.GetFeverItems(userID, sinceID, maxID, withIDs, limit)
}

// GetFeverItemCount returns the total article count visible to a user.
func (e *Engine) GetFeverItemCount(userID int64) (int, error) {
	return e.store.GetFeverItemCount(userID)
}

// GetUnreadArticleIDsForUser returns IDs of unread articles for the Fever API.
func (e *Engine) GetUnreadArticleIDsForUser(userID int64) ([]int64, error) {
	return e.store.GetUnreadArticleIDsForUser(userID)
}

// GetStarredArticleIDsForUser returns IDs of starred articles for the Fever API.
func (e *Engine) GetStarredArticleIDsForUser(userID int64) ([]int64, error) {
	return e.store.GetStarredArticleIDsForUser(userID)
}

// FeverMarkFeedRead marks feed articles as read up to the given timestamp.
func (e *Engine) FeverMarkFeedRead(userID, feedID int64, before int64) error {
	return e.store.MarkFeedArticlesRead(userID, feedID, before)
}

// FeverMarkGroupRead marks article-group articles as read up to the given timestamp.
func (e *Engine) FeverMarkGroupRead(userID, groupID int64, before int64) error {
	return e.store.MarkGroupArticlesRead(userID, groupID, before)
}

// FeverMarkAllRead marks all articles read for a user up to the given timestamp.
func (e *Engine) FeverMarkAllRead(userID int64, before int64) error {
	return e.store.MarkAllArticlesRead(userID, before)
}

// GetFeedGroupMemberships returns the Fever feeds_groups mapping.
func (e *Engine) GetFeedGroupMemberships(userID int64) (map[int64][]int64, error) {
	return e.store.GetFeedGroupMemberships(userID)
}

// MarkArticleUnread marks a single article as unread.
func (e *Engine) MarkArticleUnread(userID, articleID int64) error {
	return e.store.UpdateReadState(userID, articleID, false, nil, nil)
}

