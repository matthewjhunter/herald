package urlnorm

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		// The motivating case: tracking params, scheme, www, trailing slash all
		// normalize to the same key.
		{"https://hollymathnerd.substack.com/p/the-government-sets-the-trap", "hollymathnerd.substack.com/p/the-government-sets-the-trap"},
		{"https://hollymathnerd.substack.com/p/the-government-sets-the-trap?r=wm1qp&triedRedirect=true", "hollymathnerd.substack.com/p/the-government-sets-the-trap"},
		{"http://www.hollymathnerd.substack.com/p/the-government-sets-the-trap/", "hollymathnerd.substack.com/p/the-government-sets-the-trap"},
		{"https://EXAMPLE.com/Path#section", "example.com/Path"}, // host lowercased, path case kept, fragment dropped
		{"https://example.com", "example.com"},
		{"https://example.com/", "example.com"},
		// Non-http(s) and relative links are rejected.
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
