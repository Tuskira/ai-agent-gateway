package sink

import "testing"

// Real User-Agent strings seen from each client, plus the ambiguous/unknown
// cases that must fall through to "other" or "".
func TestClientFamily(t *testing.T) {
	cases := []struct {
		name string
		ua   string
		want string
	}{
		{"claude code cli", "claude-cli/2.1.0 (external, cli)", "claude-code"},
		{"claude code legacy family name", "claude-code/1.0", "claude-code"},
		{"cursor", "Cursor/1.5", "cursor"},
		{"vscode native mcp", "Visual Studio Code", "vscode"},
		{"vscode literal token", "vscode/1.90.0", "vscode"},
		{"codex", "codex-cli/0.3.1", "codex"},
		{"claude desktop hyphen", "claude-desktop/1.2.3", "claude-desktop"},
		{"claude desktop spaced", "Claude Desktop/1.2.3", "claude-desktop"},
		{"claude versioned slash", "Claude/1.0", "claude-desktop"},
		{"curl", "curl/8.4.0", "other"},
		{"unknown browser", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)", "other"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClientFamily(c.ua); got != c.want {
				t.Errorf("ClientFamily(%q) = %q, want %q", c.ua, got, c.want)
			}
		})
	}
}

// claude-cli must win over the desktop bucket even though it also contains
// "claude" -- order matters, and this pins it.
func TestClientFamily_ClaudeCliNotDesktop(t *testing.T) {
	if got := ClientFamily("claude-cli/2.1.0 (external, cli)"); got != "claude-code" {
		t.Errorf("got %q, want claude-code", got)
	}
}

func TestCapUserAgent(t *testing.T) {
	short := "claude-cli/2.1.0"
	if got := CapUserAgent(short); got != short {
		t.Errorf("short UA altered: got %q", got)
	}
	long := make([]byte, MaxUserAgentLen+50)
	for i := range long {
		long[i] = 'a'
	}
	got := CapUserAgent(string(long))
	if len(got) != MaxUserAgentLen {
		t.Errorf("len(capped) = %d, want %d", len(got), MaxUserAgentLen)
	}
	if got != string(long[:MaxUserAgentLen]) {
		t.Error("capped UA is not a prefix of the original")
	}
}
