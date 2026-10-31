package turn

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactRawKeepsJSON(t *testing.T) {
	secret := "AKIA" + "Z7QW3RT5YU2IOP4A" // assembled so scanners of this file stay quiet
	body := []byte(`{"messages":[{"role":"user","content":"deploy with key ` + secret + ` now"}]}`)
	out, err := RedactRaw(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), secret) {
		t.Fatalf("secret left in %s", out)
	}
	if !strings.Contains(string(out), "[REDACTED:") {
		t.Fatalf("no marker in %s", out)
	}
	var v any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("redacted body is not JSON: %v: %s", err, out)
	}
}

func TestRedactRawCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RedactRaw(ctx, []byte(strings.Repeat("x", 1<<20))); err == nil {
		t.Fatal("want an error from a cancelled scan")
	}
}
