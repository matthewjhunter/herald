package feeds

import "testing"

func TestSanitizeText_StripsControlChars(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "ANSI escape sequence stripped, visible text kept",
			in:   "before\x1b[31mRED\x1b[0mafter",
			want: "before[31mRED[0mafter",
		},
		{
			name: "preserves tab, newline, carriage return",
			in:   "a\tb\nc\rd",
			want: "a\tb\nc\rd",
		},
		{
			name: "strips NUL (PostgreSQL rejects it)",
			in:   "a\x00b",
			want: "ab",
		},
		{
			name: "strips other C0 controls (bell, backspace, vertical tab)",
			in:   "a\x07b\x08c\x0bd",
			want: "abcd",
		},
		{
			name: "strips DEL",
			in:   "a\x7fb",
			want: "ab",
		},
		{
			name: "strips C1 controls (NEL, CSI)",
			in:   "a\u0085b\u009bc",
			want: "abc",
		},
		{
			name: "preserves printable unicode",
			in:   "café 日本語 🔔 — ok",
			want: "café 日本語 🔔 — ok",
		},
		{
			name: "empty stays empty",
			in:   "",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeText(tt.in); got != tt.want {
				t.Errorf("sanitizeText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
