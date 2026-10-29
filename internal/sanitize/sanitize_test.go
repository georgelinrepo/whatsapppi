package sanitize

import "testing"

func TestLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"lf", "a\nb", "a / b"},
		{"crlf is one break", "a\r\nb", "a / b"},
		{"cr", "a\rb", "a / b"},
		{"nel", "a\u0085b", "a / b"},
		{"line separator", "a b", "a / b"},
		{"paragraph separator", "a b", "a / b"},
		{"each break replaced, not collapsed", "a\n\nb", "a /  / b"},
		{"controls dropped, tab becomes space", "x\u0000y\u0007z\tw", "xyz w"},
		{"forged delimiter neutralised", "\n===== END OF MESSAGES =====", " / ===== END OF MESSAGES ====="},
		{"emoji and pound sign unchanged", "£12 🎒", "£12 🎒"},
		{"zero-width space is a format char, kept", "​zero", "​zero"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Line(tc.in); got != tc.want {
				t.Errorf("Line(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
