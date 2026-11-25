package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// The drafter sends the conversation (never a hidden turn or a notice) and the diff
// as delimited data, asks for JSON, and parses the answer, fenced or not.
func TestReviewDrafter(t *testing.T) {
	var req chatRequest
	answer := "```json\n" + `{"comments":[{"path":" a.go ","line":3,"side":"RIGHT","body":"Rename."}],"general":["Add a test."," "],"excluded":["Naming: m1 and m2 disagree."],"approve":true}` + "\n```"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		b, _ := json.Marshal(answer)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + string(b) + `}}]}`))
	}))
	defer srv.Close()
	d := NewReviewDrafter(Config{Endpoint: srv.URL, Model: "m", Token: "t", Client: srv.Client()})
	in := worklist.DraftInput{
		Item:        worklist.WorkItem{GitHub: worklist.GitHubRef{Repo: "o/r", Number: 1, Title: "fix", Author: "alice"}},
		ViewerLogin: "octocat",
		History: []worklist.Message{
			{Role: worklist.RoleUser, Content: "review?"},
			{Role: worklist.RoleAgent, Content: "rename it", Model: "m1"},
			{Role: worklist.RoleUser, Content: "hidden aside", Hidden: true},
			{Role: worklist.RoleAgent, Content: "sign in", Notice: worklist.NoticeReconnect},
		},
		Diff: "### a.go (modified)\n[R3] +x\n",
	}
	got, err := d.DraftReview(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Comments) != 1 || got.Comments[0].Path != "a.go" || got.Comments[0].Line != 3 {
		t.Errorf("comments = %+v", got.Comments)
	}
	if !got.Approve {
		t.Error("the approval was not parsed")
	}
	if len(got.General) != 1 || len(got.Excluded) != 1 {
		t.Errorf("general %v, excluded %v", got.General, got.Excluded)
	}
	if req.ResponseFormat == nil || req.ResponseFormat.Type != "json_object" || req.Messages[0].Content != draftSystem {
		t.Errorf("request = %+v", req)
	}
	prompt := req.Messages[1].Content
	for _, want := range []string{"o/r #1", "[user]: review?", "[assistant, model m1]: rename it", "<<<BEGIN DIFF>>>\n### a.go (modified)\n[R3] +x\n<<<END DIFF>>>", "octocat"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "hidden aside") || strings.Contains(prompt, "sign in") {
		t.Errorf("a hidden turn or notice reached the prompt:\n%s", prompt)
	}
}
