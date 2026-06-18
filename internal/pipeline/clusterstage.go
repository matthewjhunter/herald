package pipeline

import (
	"context"
	"strings"

	"github.com/matthewjhunter/herald/internal/ai"
	"github.com/matthewjhunter/herald/internal/storage"
)

// Cluster is the final staged pass: it groups the cohort's articles into
// breaking-news clusters. It matches each article against a frozen snapshot of
// existing group centroids (joining ongoing stories), then links the remaining
// articles among themselves into new groups, and finally names each touched
// group with the LLM. Taking the centroid snapshot once and writing centroids
// only after every assignment is decided makes grouping deterministic and lets
// sibling articles in the same cohort form a group together — the race the old
// per-article path had.
//
// The cohort is the articles embedded this cycle plus a recency window of
// already-embedded, still-ungrouped articles (so a story that broke a few
// cycles ago still gathers late-arriving siblings).
func (s *Stage) Cluster(ctx context.Context, cohort []storage.Article) error {
	if s.Embedder == nil {
		return nil
	}
	if !s.AI.BackendAvailable() {
		s.Formatter.Warning("pipeline: skipping cluster stage for user %d — AI backend unavailable (breaker open)", s.UserID)
		return nil
	}
	if len(cohort) == 0 {
		return nil
	}
	model := s.Embedder.Model()
	// pgvector measures cosine distance (1 - cosine similarity), so the join
	// similarity threshold becomes a maximum distance (#186).
	maxDist := 1 - s.clusterThreshold()

	// Re-establish centroids for existing groups that lost theirs (the
	// BYTEA->vector reset, or a model switch) before matching against them, so a
	// recovering group can gather its ongoing story instead of spawning a
	// duplicate. No-op once centroids are current.
	s.repairCentroids(model)

	artByID := make(map[int64]storage.Article, len(cohort))
	cohortIDs := make([]int64, len(cohort))
	for i, a := range cohort {
		artByID[a.ID] = a
		cohortIDs[i] = a.ID
	}

	// 1. JOIN: in one ANN query, find each embedded cohort article's nearest
	// existing group centroid within the threshold. Matched articles join that
	// group; embedded-but-unmatched articles are held as leftovers; articles
	// with no usable embedding are absent from the result and skipped.
	matches, err := s.Store.MatchArticlesToGroups(s.UserID, model, cohortIDs, maxDist)
	if err != nil {
		return err
	}
	joined := make(map[int64]bool) // groupIDs touched by joins
	var leftoverIDs []int64
	for _, m := range matches {
		if m.GroupID != 0 {
			if s.joinArticle(m.GroupID, m.ArticleID) {
				joined[m.GroupID] = true
			}
			continue
		}
		leftoverIDs = append(leftoverIDs, m.ArticleID)
	}

	// 2. FORM: single-linkage clustering over the leftovers, with the similarity
	// edges computed in the database. Map article ids to indices so the pure
	// union-find primitive can group them.
	pairs, err := s.Store.LeftoverSimilarPairs(model, leftoverIDs, maxDist)
	if err != nil {
		return err
	}
	idxByID := make(map[int64]int, len(leftoverIDs))
	for i, id := range leftoverIDs {
		idxByID[id] = i
	}
	edges := make([][2]int, 0, len(pairs))
	for _, p := range pairs {
		if ia, oka := idxByID[p[0]]; oka {
			if ib, okb := idxByID[p[1]]; okb {
				edges = append(edges, [2]int{ia, ib})
			}
		}
	}
	minSize := s.minClusterSize()
	for _, comp := range clusterByEdges(len(leftoverIDs), edges) {
		if len(comp) < minSize {
			continue // singleton / too small — stays ungrouped
		}
		topic := truncateTopic(artByID[leftoverIDs[comp[0]]].Title)
		gid, err := s.Store.CreateArticleGroup(s.UserID, topic)
		if err != nil {
			s.Formatter.Warning("failed to create article group: %v", err)
			continue
		}
		for _, idx := range comp {
			if err := s.Store.AddArticleToGroup(gid, leftoverIDs[idx]); err != nil {
				s.Formatter.Warning("failed to add article %d to new group %d: %v", leftoverIDs[idx], gid, err)
			}
		}
		s.recomputeCentroid(gid, model)
		s.nameGroup(ctx, gid)
	}

	// 3. Refresh centroids and summaries for groups that gained joiners.
	for gid := range joined {
		s.recomputeCentroid(gid, model)
		s.nameGroup(ctx, gid)
	}
	return nil
}

// repairCentroids rebuilds the centroid of every group that is missing one (or
// whose centroid was built under a different model) but has at least one
// embedded member. It is the self-healing counterpart to dropping all centroids
// in the BYTEA->vector migration: as members re-embed over successive cycles,
// their groups regain centroids and rejoin the JOIN phase. A no-op at steady
// state (no group qualifies).
func (s *Stage) repairCentroids(model string) {
	ids, err := s.Store.GroupsNeedingCentroid(s.UserID, model)
	if err != nil {
		s.Formatter.Warning("failed to list groups needing a centroid: %v", err)
		return
	}
	for _, gid := range ids {
		s.recomputeCentroid(gid, model)
	}
}

// joinArticle adds an article to an existing group and, if that group is muted,
// immediately marks the article read so muted stories stay quiet. Returns
// whether the article was added.
func (s *Stage) joinArticle(groupID, articleID int64) bool {
	if err := s.Store.AddArticleToGroup(groupID, articleID); err != nil {
		s.Formatter.Warning("failed to add article %d to group %d: %v", articleID, groupID, err)
		return false
	}
	if muted, err := s.Store.IsGroupMuted(groupID); err == nil && muted {
		read := true
		s.Store.UpdateReadState(s.UserID, articleID, read, nil, nil, nil, nil) //nolint:errcheck
	}
	return true
}

// recomputeCentroid recomputes a group's centroid as the mean of all its
// members' embeddings, in the database via pgvector's AVG aggregate (#186). Done
// after membership changes so the centroid reflects the full group, not an
// incremental approximation.
func (s *Stage) recomputeCentroid(groupID int64, model string) {
	if err := s.Store.RecomputeGroupCentroid(groupID, model); err != nil {
		s.Formatter.Warning("failed to update centroid for group %d: %v", groupID, err)
	}
}

// nameGroup (re)generates a group's headline, narrative summary, and — once it
// has enough articles — a refined topic, via the LLM. Mirrors the standalone
// updateGroupSummary the daemon ran after grouping.
func (s *Stage) nameGroup(ctx context.Context, groupID int64) {
	arts, err := s.Store.GetGroupArticles(groupID)
	if err != nil || len(arts) == 0 {
		return
	}

	var topic string
	if g, err := s.Store.GetGroup(groupID); err == nil && g != nil {
		topic = g.Topic
	}

	ids := make([]int64, len(arts))
	for i, a := range arts {
		ids[i] = a.ID
	}
	scores, err := s.Store.GetArticleInterestScores(s.UserID, ids)
	if err != nil {
		s.Formatter.Warning("failed to get interest scores for group %d: %v", groupID, err)
		return
	}

	var inputs []ai.GroupSummaryInput
	var maxScore float64
	for _, a := range arts {
		summary, err := s.Store.GetArticleSummary(a.ID)
		if err != nil || summary == nil {
			continue
		}
		score := scores[a.ID]
		inputs = append(inputs, ai.GroupSummaryInput{Title: a.Title, AISummary: summary.AISummary, Score: score})
		if score > maxScore {
			maxScore = score
		}
	}
	if len(inputs) == 0 {
		return
	}

	res, err := s.AI.GenerateGroupSummary(ctx, s.UserID, topic, inputs)
	if err != nil {
		s.Formatter.Warning("failed to generate summary for group %d: %v", groupID, err)
		return
	}
	if err := s.Store.UpdateGroupSummary(groupID, res.Headline, res.Summary, len(arts), &maxScore); err != nil {
		s.Formatter.Warning("failed to store summary for group %d: %v", groupID, err)
		return
	}
	// Refine the topic label once the group is substantial — but only from a
	// real summary. An empty summary (degenerate/over-large cluster) would make
	// the refiner reply conversationally; skip it and keep the seed topic.
	if len(arts) >= 3 && strings.TrimSpace(res.Summary) != "" {
		if refined, err := s.AI.RefineGroupTopic(ctx, s.UserID, res.Summary); err == nil && refined != "" {
			s.Store.UpdateGroupTopic(groupID, refined) //nolint:errcheck
		}
	}
}

// truncateTopic caps a seed topic (an article title) at 100 characters, the
// same bound the engine grouping path used.
func truncateTopic(title string) string {
	if len(title) > 100 {
		return title[:100]
	}
	return title
}

func (s *Stage) clusterThreshold() float64 {
	if s.Cfg.Grouping.ClusterThreshold > 0 {
		return s.Cfg.Grouping.ClusterThreshold
	}
	return s.Cfg.Grouping.SimilarityThreshold
}

func (s *Stage) minClusterSize() int {
	if s.Cfg.Grouping.MinClusterSize > 0 {
		return s.Cfg.Grouping.MinClusterSize
	}
	return 2
}
