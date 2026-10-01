package store

import (
	"errors"
	"regexp"
	"strings"
)

var usernameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._@+-]{2,63}$`)

// NormalizeUsername trims and lowercases a username as typed.
func NormalizeUsername(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// ValidateUsername normalizes raw and checks it against the username
// rules (the same ones the users table documents): 3-64 characters,
// lowercase letters, digits and . _ @ + -, starting with a letter or
// digit, so an email address is a valid username. It returns the
// normalized username.
func ValidateUsername(raw string) (string, error) {
	u := NormalizeUsername(raw)
	if !usernameRE.MatchString(u) {
		return "", errors.New("username must be 3-64 characters: lowercase letters, digits and . _ @ + -, starting with a letter or digit")
	}
	return u, nil
}
