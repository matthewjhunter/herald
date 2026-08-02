package herald

import (
	"testing"
)

// TestRevertPromptRejectsOtherUsersVersion is the access check on the revert
// path (#258). A version id is a bare integer in a URL, so without an ownership
// test, guessing one would copy another account's prompt text into the caller's
// own -- a read primitive for other people's prompts dressed up as a revert.
func TestRevertPromptRejectsOtherUsersVersion(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	alice, err := engine.store.CreateUser("alice")
	if err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	bob, err := engine.store.CreateUser("bob")
	if err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}

	const secret = "alice's private curation prompt"
	if err := engine.SetPrompt(alice, "curation", secret, nil, nil); err != nil {
		t.Fatalf("SetPrompt alice: %v", err)
	}
	aliceHistory, err := engine.ListPromptHistory(alice, "curation", 10)
	if err != nil {
		t.Fatalf("ListPromptHistory alice: %v", err)
	}
	if len(aliceHistory) != 1 {
		t.Fatalf("got %d versions for alice, want 1", len(aliceHistory))
	}

	if err := engine.RevertPrompt(bob, "curation", aliceHistory[0].ID); err == nil {
		t.Fatal("bob reverted to alice's prompt version; want an error")
	}

	// And nothing leaked into bob's account.
	bobDetail, err := engine.GetPrompt(bob, "curation")
	if err != nil {
		t.Fatalf("GetPrompt bob: %v", err)
	}
	if bobDetail.Template == secret {
		t.Error("alice's prompt text ended up in bob's account")
	}
	bobHistory, err := engine.ListPromptHistory(bob, "curation", 10)
	if err != nil {
		t.Fatalf("ListPromptHistory bob: %v", err)
	}
	if len(bobHistory) != 0 {
		t.Errorf("bob has %d versions, want 0", len(bobHistory))
	}
}

// Reverting restores the older text and records the revert as its own version.
func TestRevertPromptRestoresAndAppends(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	uid, err := engine.store.CreateUser("reader")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if err := engine.SetPrompt(uid, "curation", "original prompt", nil, nil); err != nil {
		t.Fatalf("SetPrompt original: %v", err)
	}
	if err := engine.SetPrompt(uid, "curation", "regrettable rewrite", nil, nil); err != nil {
		t.Fatalf("SetPrompt rewrite: %v", err)
	}

	history, err := engine.ListPromptHistory(uid, "curation", 10)
	if err != nil {
		t.Fatalf("ListPromptHistory: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("got %d versions, want 2", len(history))
	}
	// history[0] is the rewrite (newest); history[1] is the original.
	if !history[0].Current {
		t.Error("newest version is not marked current")
	}

	if err := engine.RevertPrompt(uid, "curation", history[1].ID); err != nil {
		t.Fatalf("RevertPrompt: %v", err)
	}

	detail, err := engine.GetPrompt(uid, "curation")
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	if detail.Template != "original prompt" {
		t.Errorf("template = %q, want the reverted-to text", detail.Template)
	}

	after, err := engine.ListPromptHistory(uid, "curation", 10)
	if err != nil {
		t.Fatalf("ListPromptHistory after revert: %v", err)
	}
	if len(after) != 3 {
		t.Errorf("got %d versions after revert, want 3 -- the revert is an event, and the rejected version must survive", len(after))
	}
	// The rewrite is still on the record even though it is no longer in force:
	// scores written under it must stay attributable.
	var sawRewrite bool
	for _, v := range after {
		if v.Template == "regrettable rewrite" {
			sawRewrite = true
		}
	}
	if !sawRewrite {
		t.Error("the reverted-away version was erased from history")
	}
}
