package majordomo

import (
	"fmt"
	"strings"

	"github.com/feedreader/feedreader/internal/storage"
)

type Notifier struct {
	enabled       bool
	chatCommand   string
	targetPersona string
}

// NewNotifier creates a new Majordomo notifier
func NewNotifier(enabled bool, chatCommand, targetPersona string) *Notifier {
	return &Notifier{
		enabled:       enabled,
		chatCommand:   chatCommand,
		targetPersona: targetPersona,
	}
}

// NotifyHighInterestArticles sends high-interest articles to Majordomo
func (n *Notifier) NotifyHighInterestArticles(articles []storage.Article, scores []float64) error {
	if !n.enabled {
		return nil
	}

	if len(articles) == 0 {
		return nil
	}

	// Build notification message
	var messages []string
	for i, article := range articles {
		score := scores[i]
		msg := fmt.Sprintf("📰 High Interest Article (%.1f/10)\n\nTitle: %s\n\nURL: %s\n\nSummary: %s",
			score, article.Title, article.URL, truncate(article.Summary, 200))
		messages = append(messages, msg)
	}

	// Send each article as a separate message
	for _, msg := range messages {
		if err := n.sendChatMessage(msg); err != nil {
			return fmt.Errorf("failed to send notification: %w", err)
		}
	}

	return nil
}

// sendChatMessage sends a message via majordomo
func (n *Notifier) sendChatMessage(message string) error {
	// TODO: When majordomo CLI chat command is implemented, use:
	// cmd := exec.Command(n.chatCommand, "chat", "--recipients", n.targetPersona, "--text", message)
	// For now, output to stdout in a format that could be captured

	fmt.Println("╔════════════════════════════════════════════════════════════════════════")
	fmt.Printf("║ 🔔 MAJORDOMO NOTIFICATION → %s\n", n.targetPersona)
	fmt.Println("╠════════════════════════════════════════════════════════════════════════")
	fmt.Println(message)
	fmt.Println("╚════════════════════════════════════════════════════════════════════════")

	return nil
}

// truncate truncates a string to maxLen characters
func truncate(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
