package pipeline

import (
	"context"
	"strings"

	embedding "github.com/matthewjhunter/go-embedding"
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
	threshold := s.clusterThreshold()

	// Load the cohort's vectors in one query.
	cohortIDs := make([]int64, len(cohort))
	for i, a := range cohort {
		cohortIDs[i] = a.ID
	}
	embRows, err := s.Store.GetArticleEmbeddingsByIDs(cohortIDs, model)
	if err != nil {
		return err
	}
	vecByID := make(map[int64][]float32, len(embRows))
	for _, r := range embRows {
		vecByID[r.ArticleID] = embedding.DecodeFloat32s(r.Embedding)
	}

	// Snapshot existing group centroids once; all join decisions are made
	// against this frozen set.
	groups, err := s.Store.GetGroupsWithEmbeddings(s.UserID, model)
	if err != nil {
		return err
	}
	snapIDs := make([]int64, 0, len(groups))
	snapVecs := make([][]float32, 0, len(groups))
	for _, g := range groups {
		snapIDs = append(snapIDs, g.ID)
		snapVecs = append(snapVecs, embedding.DecodeFloat32s(g.Embedding))
	}

	// 1. JOIN: assign each cohort article to its best existing group if similar
	// enough; otherwise hold it as a leftover for new-group formation.
	joined := make(map[int64]bool) // groupIDs touched by joins
	var leftoverArts []storage.Article
	var leftoverVecs [][]float32
	for _, a := range cohort {
		vec, ok := vecByID[a.ID]
		if !ok {
			continue // no usable embedding — skip
		}
		if len(snapVecs) > 0 {
			if idx, sim := bestCentroidMatch(vec, snapVecs); idx >= 0 && sim >= threshold {
				gid := snapIDs[idx]
				if s.joinArticle(gid, a.ID) {
					joined[gid] = true
				}
				continue
			}
		}
		leftoverArts = append(leftoverArts, a)
		leftoverVecs = append(leftoverVecs, vec)
	}

	// 2. FORM: link leftovers into new groups (>= MinClusterSize members).
	minSize := s.minClusterSize()
	for _, comp := range clusterByEdges(len(leftoverVecs), cosineEdges(leftoverVecs, threshold)) {
		if len(comp) < minSize {
			continue // singleton / too small — stays ungrouped
		}
		topic := truncateTopic(leftoverArts[comp[0]].Title)
		gid, err := s.Store.CreateArticleGroup(s.UserID, topic)
		if err != nil {
			s.Formatter.Warning("failed to create article group: %v", err)
			continue
		}
		for _, idx := range comp {
			if err := s.Store.AddArticleToGroup(gid, leftoverArts[idx].ID); err != nil {
				s.Formatter.Warning("failed to add article %d to new group %d: %v", leftoverArts[idx].ID, gid, err)
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
// members' embeddings and stores it. Done after membership changes so the
// centroid reflects the full group, not an incremental approximation.
func (s *Stage) recomputeCentroid(groupID int64, model string) {
	arts, err := s.Store.GetGroupArticles(groupID)
	if err != nil || len(arts) == 0 {
		return
	}
	ids := make([]int64, len(arts))
	for i, a := range arts {
		ids[i] = a.ID
	}
	rows, err := s.Store.GetArticleEmbeddingsByIDs(ids, model)
	if err != nil || len(rows) == 0 {
		return
	}
	vecs := make([][]float32, 0, len(rows))
	for _, r := range rows {
		vecs = append(vecs, embedding.DecodeFloat32s(r.Embedding))
	}
	centroid := meanCentroid(vecs)
	if centroid == nil {
		return
	}
	if err := s.Store.UpdateGroupEmbedding(groupID, embedding.EncodeFloat32s(centroid), model); err != nil {
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
