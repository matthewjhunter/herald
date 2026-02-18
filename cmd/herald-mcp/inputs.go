package main

// Input types for MCP tools. The SDK infers JSON Schema from these structs.
// Pointer types are optional; value types are required.

type articlesUnreadInput struct {
	Limit    *int     `json:"limit,omitempty"     jsonschema:"Maximum number of articles to return (default 20)"`
	Offset   *int     `json:"offset,omitempty"    jsonschema:"Number of articles to skip for pagination (default 0)"`
	MinScore *float64 `json:"min_score,omitempty" jsonschema:"Minimum interest score filter (0-10). Only returns articles scored at or above this threshold."`
	Speaker  *string  `json:"speaker,omitempty"   jsonschema:"Speaker name for multi-user resolution. If omitted uses the default user."`
}

type articleIDInput struct {
	ArticleID int64   `json:"article_id"           jsonschema:"The article ID"`
	Speaker   *string `json:"speaker,omitempty"     jsonschema:"Speaker name for multi-user resolution. If omitted uses the default user."`
}

type speakerOnlyInput struct {
	Speaker *string `json:"speaker,omitempty" jsonschema:"Speaker name for multi-user resolution. If omitted uses the default user."`
}

type feedSubscribeInput struct {
	URL     string  `json:"url"                jsonschema:"The RSS/Atom feed URL"`
	Title   *string `json:"title,omitempty"    jsonschema:"Optional display title for the feed. If omitted the feed's own title is used once fetched."`
	Speaker *string `json:"speaker,omitempty"  jsonschema:"Speaker name for multi-user resolution. If omitted uses the default user."`
}

type feedIDInput struct {
	FeedID  int64   `json:"feed_id"            jsonschema:"The feed ID"`
	Speaker *string `json:"speaker,omitempty"  jsonschema:"Speaker name for multi-user resolution. If omitted uses the default user."`
}

type feedRenameInput struct {
	FeedID int64  `json:"feed_id" jsonschema:"The feed ID to rename"`
	Title  string `json:"title"   jsonschema:"The new display title"`
}

type articleGroupGetInput struct {
	GroupID int64   `json:"group_id"           jsonschema:"The group ID to retrieve"`
	Speaker *string `json:"speaker,omitempty"  jsonschema:"Speaker name for multi-user resolution. If omitted uses the default user."`
}

type preferenceSetInput struct {
	Key     string  `json:"key"               jsonschema:"Preference key to set. Valid keys: keywords, interest_threshold, notify_when, notify_min_score"`
	Value   string  `json:"value"             jsonschema:"Value to set (keywords as JSON array, thresholds as number strings, notify_when as enum)"`
	Speaker *string `json:"speaker,omitempty" jsonschema:"Speaker name for multi-user resolution. If omitted uses the default user."`
}

type promptTypeInput struct {
	PromptType string  `json:"prompt_type"        jsonschema:"The prompt type. Valid types: curation, summarization, group_summary, related_groups"`
	Speaker    *string `json:"speaker,omitempty"   jsonschema:"Speaker name for multi-user resolution. If omitted uses the default user."`
}

type promptSetInput struct {
	PromptType  string   `json:"prompt_type"            jsonschema:"The prompt type to customize. Valid types: curation, summarization, group_summary, related_groups"`
	Template    *string  `json:"template,omitempty"     jsonschema:"New prompt template text"`
	Temperature *float64 `json:"temperature,omitempty"  jsonschema:"Temperature setting (0.0-2.0)"`
	Speaker     *string  `json:"speaker,omitempty"      jsonschema:"Speaker name for multi-user resolution. If omitted uses the default user."`
}

type articleStarInput struct {
	ArticleID int64   `json:"article_id"              jsonschema:"The article ID to star/unstar"`
	Starred   *bool   `json:"starred,omitempty"       jsonschema:"true to star, false to unstar"`
	Speaker   *string `json:"speaker,omitempty"       jsonschema:"Speaker name for multi-user resolution. If omitted uses the default user."`
}

type userRegisterInput struct {
	Name string `json:"name" jsonschema:"Speaker name to register"`
}

type filterRulesListInput struct {
	FeedID  *int64  `json:"feed_id,omitempty"  jsonschema:"Optional feed ID to filter rules by. If omitted returns all rules."`
	Speaker *string `json:"speaker,omitempty"   jsonschema:"Speaker name for multi-user resolution. If omitted uses the default user."`
}

type filterRuleAddInput struct {
	Axis    string  `json:"axis"               jsonschema:"Filter axis: author, category, or tag"`
	Value   string  `json:"value"              jsonschema:"Value to match (e.g. author name, category name)"`
	Score   int     `json:"score"              jsonschema:"Score to add when this rule matches (positive = boost, negative = penalize)"`
	FeedID  *int64  `json:"feed_id,omitempty"  jsonschema:"Optional feed ID to scope this rule to a single feed. If omitted the rule is global."`
	Speaker *string `json:"speaker,omitempty"   jsonschema:"Speaker name for multi-user resolution. If omitted uses the default user."`
}

type filterRuleUpdateInput struct {
	RuleID  int64   `json:"rule_id"            jsonschema:"The filter rule ID to update"`
	Score   int     `json:"score"              jsonschema:"New score value"`
	Speaker *string `json:"speaker,omitempty"   jsonschema:"Speaker name for multi-user resolution. If omitted uses the default user."`
}

type filterRuleDeleteInput struct {
	RuleID  int64   `json:"rule_id"            jsonschema:"The filter rule ID to delete"`
	Speaker *string `json:"speaker,omitempty"   jsonschema:"Speaker name for multi-user resolution. If omitted uses the default user."`
}

type feedMetadataInput struct {
	FeedID  int64   `json:"feed_id"            jsonschema:"The feed ID to discover metadata for"`
	Speaker *string `json:"speaker,omitempty"   jsonschema:"Speaker name for multi-user resolution. If omitted uses the default user."`
}

type emptyInput struct{}
