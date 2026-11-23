package ingest

import (
	"context"
	"testing"
)

// Every run carries the bot list (run parameter bots), so enrich never takes a bot's
// comment for the last human word; a malformed entry is dropped, not split.
func TestRunsCarryTheBots(t *testing.T) {
	d := &lockedDispatcher{}
	ig := New(d, "gofer-agent", "http://gofer/sink", nil, "", "")
	ig.UseBots([]string{" k8s-ci-robot ", "", "a,b", "netlify"})
	if err := ig.Refresh(context.Background(), "github:1"); err != nil {
		t.Fatal(err)
	}
	if got := d.specs[0].Parameters["bots"]; got != "k8s-ci-robot,netlify" {
		t.Fatalf("bots = %q, want k8s-ci-robot,netlify", got)
	}

	d = &lockedDispatcher{}
	if err := New(d, "gofer-agent", "http://gofer/sink", nil, "", "").Refresh(context.Background(), "github:1"); err != nil {
		t.Fatal(err)
	}
	if v, ok := d.specs[0].Parameters["bots"]; ok {
		t.Fatalf("bots = %q with none configured, want unset", v)
	}
}
