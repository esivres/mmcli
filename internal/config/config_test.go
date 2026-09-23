package config

import "testing"

// A current name pointing at a removed context must not block a new login
// from becoming current, otherwise every command fails with "not found".
func TestSetReplacesDanglingCurrent(t *testing.T) {
	c := &Config{CurrentName: "gone", Contexts: map[string]Context{}}
	c.Set("bot", Context{URL: "https://mm.example.com"})
	if c.CurrentName != "bot" {
		t.Fatalf("current = %q, want bot", c.CurrentName)
	}
	c.Set("work", Context{URL: "https://mm.example.com"})
	if c.CurrentName != "bot" {
		t.Fatalf("adding a context switched current to %q", c.CurrentName)
	}
}
