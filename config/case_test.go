package config

import "testing"

// TestSnakeCase covers the two transitions the flag names depend on: a
// lower-to-upper boundary, and the end of an acronym. Both appear constantly
// in real config structs (MaxAttempts, HTTPServer), and a change here renames
// flags in every adopting service at once.
func TestSnakeCase(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in, want string
	}{
		{"", ""},
		{"A", "a"},
		{"MaxAttempts", "max_attempts"},
		{"ID", "id"},
		{"APIKey", "api_key"},
		{"HTTPServer", "http_server"},
		{"ServerHTTP", "server_http"},
		{"DataDir", "data_dir"},
		{"S3Bucket", "s3bucket"},
		{"lowercase", "lowercase"},
	} {
		if got := snakeCase(tc.in); got != tc.want {
			t.Errorf("snakeCase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHyphenate(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in, want string
	}{
		{"", ""},
		{"max_attempts", "max-attempts"},
		{"already-hyphenated", "already-hyphenated"},
		{"a_b_c", "a-b-c"},
	} {
		if got := hyphenate(tc.in); got != tc.want {
			t.Errorf("hyphenate(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
