package storage

import (
	"testing"
)

func mustUser(t *testing.T, store Store) int64 {
	t.Helper()
	uid, err := store.CreateUser("reader")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return uid
}

// TestSetUserPromptAppendsVersion is the core guarantee of #258: an edit must
// not destroy the text it replaced.
func TestSetUserPromptAppendsVersion(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	uid := mustUser(t, s)

	if err := s.SetUserPrompt(uid, "curation", "first draft", nil, nil); err != nil {
		t.Fatalf("set first: %v", err)
	}
	if err := s.SetUserPrompt(uid, "curation", "second draft", nil, nil); err != nil {
		t.Fatalf("set second: %v", err)
	}

	versions, err := s.ListPromptVersions(uid, "curation", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("got %d versions, want 2", len(versions))
	}
	// Newest first.
	if versions[0].Template != "second draft" {
		t.Errorf("newest = %q, want %q", versions[0].Template, "second draft")
	}
	if versions[1].Template != "first draft" {
		t.Errorf("oldest = %q, want %q -- the overwritten text must survive", versions[1].Template, "first draft")
	}
	for _, v := range versions {
		if v.Source != SourceUser {
			t.Errorf("source = %q, want %q", v.Source, SourceUser)
		}
		if v.TemplateHash != HashPromptTemplate(v.Template) {
			t.Errorf("hash %q does not match its own template", v.TemplateHash)
		}
	}
}

// A revert appends rather than rewinding, so history stays a truthful record of
// what was in force and when.
func TestRevertAppendsRatherThanRewinds(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	uid := mustUser(t, s)

	for _, text := range []string{"A", "B", "A"} {
		if err := s.SetUserPrompt(uid, "curation", text, nil, nil); err != nil {
			t.Fatalf("set %q: %v", text, err)
		}
	}

	versions, err := s.ListPromptVersions(uid, "curation", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("got %d versions, want 3 -- a revert is an event, not an erasure", len(versions))
	}
	if versions[0].Template != "A" || versions[1].Template != "B" {
		t.Errorf("history = %q,%q,... want A,B,A newest-first",
			versions[0].Template, versions[1].Template)
	}
	// Same text, same content address, distinct rows.
	if versions[0].TemplateHash != versions[2].TemplateHash {
		t.Error("identical text produced different hashes")
	}
	if versions[0].ID == versions[2].ID {
		t.Error("revert reused the original row instead of appending")
	}
}

// RegisterPromptVersion backs the builtin and config tiers, which resolve on
// every process start. It must not accumulate a row each time.
func TestRegisterPromptVersionIsIdempotent(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	v := PromptVersion{
		UserID:       0,
		PromptType:   "curation",
		Template:     "built-in text",
		TemplateHash: HashPromptTemplate("built-in text"),
		Source:       SourceBuiltin,
	}
	for i := 0; i < 3; i++ {
		if err := s.RegisterPromptVersion(v); err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
	}

	versions, err := s.ListPromptVersions(0, "curation", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("got %d rows after 3 registrations, want 1", len(versions))
	}
	if versions[0].Source != SourceBuiltin {
		t.Errorf("source = %q, want %q", versions[0].Source, SourceBuiltin)
	}
}

// A hash recorded on a score or a feedback event must be resolvable back to the
// text, or provenance is just an opaque string.
func TestGetPromptTemplateByHash(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	uid := mustUser(t, s)

	const text = "score this article"
	if err := s.SetUserPrompt(uid, "curation", text, nil, nil); err != nil {
		t.Fatalf("set: %v", err)
	}

	got, err := s.GetPromptTemplateByHash(HashPromptTemplate(text))
	if err != nil {
		t.Fatalf("by hash: %v", err)
	}
	if got != text {
		t.Errorf("got %q, want %q", got, text)
	}

	// An unknown hash is the normal answer for scores written before
	// provenance existed, and must not be an error.
	missing, err := s.GetPromptTemplateByHash(HashPromptTemplate("never saved"))
	if err != nil {
		t.Fatalf("unknown hash errored: %v", err)
	}
	if missing != "" {
		t.Errorf("unknown hash returned %q, want empty", missing)
	}
}

// A version id is a bare integer in a URL. Reverting to another user's version
// must fail rather than copying their prompt text into the caller's account,
// which would turn the revert path into a read primitive for other people's
// prompts.
func TestPromptVersionScopeIsCheckable(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	alice := mustUser(t, s)
	bob, err := s.CreateUser("bob")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if err := s.SetUserPrompt(alice, "curation", "alice private prompt", nil, nil); err != nil {
		t.Fatalf("set alice: %v", err)
	}
	aliceVersions, err := s.ListPromptVersions(alice, "curation", 10)
	if err != nil {
		t.Fatalf("list alice: %v", err)
	}
	if len(aliceVersions) != 1 {
		t.Fatalf("got %d alice versions, want 1", len(aliceVersions))
	}

	// Bob's own history must not contain it.
	bobVersions, err := s.ListPromptVersions(bob, "curation", 10)
	if err != nil {
		t.Fatalf("list bob: %v", err)
	}
	if len(bobVersions) != 0 {
		t.Fatalf("bob sees %d versions, want 0", len(bobVersions))
	}

	// Fetching by id still returns the row -- the store is not the access
	// boundary -- so it must carry the owner for the caller to check.
	got, err := s.GetPromptVersion(aliceVersions[0].ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got == nil {
		t.Fatal("version not found by id")
	}
	if got.UserID != alice {
		t.Errorf("UserID = %d, want %d -- ownership must be checkable by the caller", got.UserID, alice)
	}
}
