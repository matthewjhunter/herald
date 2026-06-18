package urlnorm

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"https://hollymathnerd.substack.com/p/the-government-sets-the-trap", "hollymathnerd.substack.com/p/the-government-sets-the-trap"},
		{"https://hollymathnerd.substack.com/p/the-government-sets-the-trap?r=wm1qp&triedRedirect=true", "hollymathnerd.substack.com/p/the-government-sets-the-trap"},
		{"http://www.hollymathnerd.substack.com/p/the-government-sets-the-trap/", "hollymathnerd.substack.com/p/the-government-sets-the-trap"},
		{"https://EXAMPLE.com/Path#section", "example.com/path"}, // host AND path lower-cased, fragment dropped
		{"https://example.com", "example.com"},
		{"https://example.com/", "example.com"},
		{"mailto:a@b.com", ""},
		{"javascript:void(0)", ""},
		{"/relative/path", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := Normalize(c.raw); got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestQueryKey(t *testing.T) {
	cases := []struct{ raw, want string }{
		// Full URLs normalize to the same key as Normalize.
		{"https://Example.com/Foo?utm=1#x", "example.com/foo"},
		{"http://www.example.com/foo/", "example.com/foo"},
		// Bare domain and partial path are accepted (prefix searches).
		{"example.com", "example.com"},
		{"example.com/section", "example.com/section"},
		{"EXAMPLE.COM/Section/", "example.com/section"},
		// Not host-like -> "" (falls back to full-text search).
		{"openai", ""},
		{"golang generics", ""},
		{"", ""},
		{"  ", ""},
	}
	for _, c := range cases {
		if got := QueryKey(c.raw); got != c.want {
			t.Errorf("QueryKey(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}
