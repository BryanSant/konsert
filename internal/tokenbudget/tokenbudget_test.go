package tokenbudget

import "testing"

func TestEstimate(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"ab", 1},
		{"abcd", 1},
		{"abcde", 2},
		{"12345678", 2},
		{"123456789", 3},
	}
	for _, tc := range cases {
		if got := Estimate(tc.in); got != tc.want {
			t.Errorf("Estimate(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
