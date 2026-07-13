package main

import "testing"

// band must match the pipeline's threat bands exactly (pass <= 3, borderline <= 6,
// fail above), or the harness's classification-shift count is measuring a
// different policy than the one that will run.
func TestBandClassification(t *testing.T) {
	const passT, borderT = 3.0, 6.0
	cases := []struct {
		threat float64
		want   string
	}{
		{0, "pass"},
		{3.0, "pass"}, // boundary: <= passT is a pass
		{3.01, "border"},
		{6.0, "border"}, // boundary: <= borderT is borderline
		{6.01, "fail"},
		{10, "fail"},
	}
	for _, c := range cases {
		if got := band(c.threat, passT, borderT); got != c.want {
			t.Errorf("band(%.2f) = %q, want %q", c.threat, got, c.want)
		}
	}
}
