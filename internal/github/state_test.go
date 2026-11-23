package github

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// The radar draws each item in the state GitHub does, so a search hit's draft flag,
// merge time and close reason must reach the GitHubRef. draft means nothing on an
// issue and is never recorded there.
func TestSearchItemRecordsState(t *testing.T) {
	cases := []struct {
		name, payload string
		want          worklist.GitHubRef
	}{
		{"open PR", `{"state":"open","draft":false,"pull_request":{"url":"x","merged_at":null}}`, worklist.GitHubRef{State: "open"}},
		{"draft PR", `{"state":"open","draft":true,"pull_request":{"url":"x"}}`, worklist.GitHubRef{State: "open", Draft: true}},
		{"merged PR", `{"state":"closed","pull_request":{"url":"x","merged_at":"2026-01-02T00:00:00Z"}}`, worklist.GitHubRef{State: "closed", Merged: true}},
		{"closed PR", `{"state":"closed","pull_request":{"url":"x","merged_at":null}}`, worklist.GitHubRef{State: "closed"}},
		{"not planned issue", `{"state":"closed","state_reason":"not_planned"}`, worklist.GitHubRef{State: "closed", StateReason: "not_planned"}},
		{"issue claiming draft", `{"state":"open","draft":true}`, worklist.GitHubRef{State: "open"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var it searchItem
			if err := json.Unmarshal([]byte(tc.payload), &it); err != nil {
				t.Fatal(err)
			}
			got, ok := it.toWorkItem(worklist.ReasonInvolved, "me", time.Now())
			if !ok {
				t.Fatal("item dropped")
			}
			g := got.GitHub
			if g.State != tc.want.State || g.Draft != tc.want.Draft || g.Merged != tc.want.Merged || g.StateReason != tc.want.StateReason {
				t.Fatalf("got state=%q draft=%v merged=%v reason=%q, want %+v", g.State, g.Draft, g.Merged, g.StateReason, tc.want)
			}
		})
	}
}
