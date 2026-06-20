package urlnorm

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"https://hollymathnerd.substack.com/p/the-government-sets-the-trap", "hollymathnerd.substack.com/p/the-government-sets-the-trap"},
		{"https://hollymathnerd.substack.com/p/the-government-sets-the-trap?r=wm1qp&triedRedirect=true", "hollymathnerd.substack.com/p/the-government-sets-the-trap"}, // path present -> query (tracking) dropped
		{"http://www.hollymathnerd.substack.com/p/the-government-sets-the-trap/", "hollymathnerd.substack.com/p/the-government-sets-the-trap"},
		{"https://EXAMPLE.com/Path#section", "example.com/path"}, // host AND path lower-cased, fragment dropped
		{"https://example.com", "example.com"},
		{"https://example.com/", "example.com"},
		// Path-empty query carries article identity (WordPress ?p= permalinks):
		// kept so distinct posts get distinct keys instead of collapsing to host.
		{"https://www.battleswarmblog.com/?p=58410", "battleswarmblog.com?p=58410"},
		{"https://battleswarmblog.com/?p=58410", "battleswarmblog.com?p=58410"},
		{"https://battleswarmblog.com/?p=99999", "battleswarmblog.com?p=99999"}, // distinct from 58410
		{"https://example.com/?b=2&a=1", "example.com?a=1&b=2"},                 // kept query is sorted+lower-cased
		{"https://example.com/article?utm_source=x", "example.com/article"},     // path present -> tracking query still dropped
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

func TestHost(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"https://example.com/p/123", "example.com"},
		{"http://www.Example.com/Path", "example.com"}, // www stripped, lower-cased
		{"https://sub.example.com", "sub.example.com"}, // non-www subdomain kept
		{"https://example.com:8080/x", "example.com:8080"},
		{"mailto:a@b.com", ""},
		{"/relative", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := Host(c.raw); got != c.want {
			t.Errorf("Host(%q) = %q, want %q", c.raw, got, c.want)
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
