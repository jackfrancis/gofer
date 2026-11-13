package storage

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackfrancis/gofer/internal/worklist"
)

// worklistStore is what both implementations provide.
type worklistStore interface {
	worklist.Store
	worklist.Lister
}

// worklistImpls are the implementations the parity table runs against: the
// in-memory reference and SQLite. Any behavior asserted there is asserted of both.
func worklistImpls(t *testing.T) map[string]worklistStore {
	return map[string]worklistStore{
		"memory": worklist.NewMemoryStore(),
		"sqlite": open(t, t.TempDir()).Worklist(),
	}
}

func item(id, title string) worklist.WorkItem {
	return worklist.WorkItem{
		ID:     id,
		Source: "github",
		Type:   worklist.TypePullRequest,
		GitHub: worklist.GitHubRef{Number: 7, Repo: "o/r", Title: title, UpdatedAt: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)},
		Signals: worklist.Signals{
			Reasons:  []worklist.Reason{worklist.ReasonReviewRequested},
			Comments: 3,
			Proposed: nil,
		},
		Meta: worklist.Metadata{Rank: 0.5, Priority: worklist.PriorityHigh, Origin: worklist.OriginAgent},
		Thread: []worklist.Message{
			{Role: worklist.RoleUser, Content: "review this", Kind: worklist.KindReviewRequest, At: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)},
			{Role: worklist.RoleAgent, Content: "LGTM — \"quoted\" ünïcode\n", Model: "m", At: time.Date(2026, 11, 2, 9, 1, 0, 0, time.UTC)},
		},
		CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}
}

// same compares items by their JSON documents: that is the stored form, and it
// treats a nil and an empty omitempty slice alike (see ADR 0004).
func same(t *testing.T, got, want []worklist.WorkItem) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Fatalf("items differ\n got: %s\nwant: %s", g, w)
	}
}

func scoped(owner string, items ...worklist.WorkItem) []worklist.WorkItem {
	out := make([]worklist.WorkItem, len(items))
	for i, it := range items {
		it.OwnerID = owner
		out[i] = it
	}
	return out
}

func TestWorklistParity(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, s worklistStore)
	}{
		{"an unknown owner lists as empty, not nil", func(t *testing.T, s worklistStore) {
			got, err := s.List(ctx, "nobody")
			if err != nil {
				t.Fatal(err)
			}
			if got == nil || len(got) != 0 {
				t.Fatalf("got %#v, want an empty non-nil slice", got)
			}
		}},
		{"upsert then list round-trips the whole document", func(t *testing.T, s worklistStore) {
			a := item("a", "first")
			if err := s.Upsert(ctx, "alice", a); err != nil {
				t.Fatal(err)
			}
			got, _ := s.List(ctx, "alice")
			same(t, got, scoped("alice", a))
		}},
		{"list returns first-insertion order", func(t *testing.T, s worklistStore) {
			// Deliberately not sorted by id, and written in two calls.
			_ = s.Upsert(ctx, "alice", item("c", "1"), item("a", "2"))
			_ = s.Upsert(ctx, "alice", item("b", "3"))
			got, _ := s.List(ctx, "alice")
			same(t, got, scoped("alice", item("c", "1"), item("a", "2"), item("b", "3")))
		}},
		{"upsert replaces by id and keeps the item's position", func(t *testing.T, s worklistStore) {
			_ = s.Upsert(ctx, "alice", item("a", "old"), item("b", "keep"))
			_ = s.Upsert(ctx, "alice", item("a", "new"), item("c", "added"))
			got, _ := s.List(ctx, "alice")
			same(t, got, scoped("alice", item("a", "new"), item("b", "keep"), item("c", "added")))
		}},
		{"a replacement is whole-document: dropped fields do not linger", func(t *testing.T, s worklistStore) {
			_ = s.Upsert(ctx, "alice", item("a", "t"))
			bare := worklist.WorkItem{ID: "a"}
			_ = s.Upsert(ctx, "alice", bare)
			got, _ := s.List(ctx, "alice")
			same(t, got, scoped("alice", bare))
		}},
		{"the last duplicate id in one call wins", func(t *testing.T, s worklistStore) {
			_ = s.Upsert(ctx, "alice", item("a", "first"), item("a", "second"))
			got, _ := s.List(ctx, "alice")
			same(t, got, scoped("alice", item("a", "second")))
		}},
		{"items are re-scoped to the owner on write", func(t *testing.T, s worklistStore) {
			forged := item("a", "t")
			forged.OwnerID = "bob" // an agent claiming another user's item
			_ = s.Upsert(ctx, "alice", forged)
			got, _ := s.List(ctx, "alice")
			if len(got) != 1 || got[0].OwnerID != "alice" {
				t.Fatalf("alice's items = %+v", got)
			}
			if bob, _ := s.List(ctx, "bob"); len(bob) != 0 {
				t.Fatalf("the write crossed to bob: %+v", bob)
			}
		}},
		{"owners are isolated: the same id is a different item per owner", func(t *testing.T, s worklistStore) {
			_ = s.Upsert(ctx, "alice", item("a", "alice's"))
			_ = s.Upsert(ctx, "bob", item("a", "bob's"))
			_ = s.Upsert(ctx, "alice", item("a", "alice's, edited"))
			alice, _ := s.List(ctx, "alice")
			bob, _ := s.List(ctx, "bob")
			same(t, alice, scoped("alice", item("a", "alice's, edited")))
			same(t, bob, scoped("bob", item("a", "bob's")))
		}},
		{"All returns every owner's items", func(t *testing.T, s worklistStore) {
			_ = s.Upsert(ctx, "alice", item("a", "1"), item("b", "2"))
			_ = s.Upsert(ctx, "bob", item("z", "3"))
			all, err := s.All(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(all) != 2 {
				t.Fatalf("owners = %d, want 2", len(all))
			}
			same(t, all["alice"], scoped("alice", item("a", "1"), item("b", "2")))
			same(t, all["bob"], scoped("bob", item("z", "3")))
		}},
		{"a returned slice is a copy", func(t *testing.T, s worklistStore) {
			_ = s.Upsert(ctx, "alice", item("a", "stored"))
			got, _ := s.List(ctx, "alice")
			got[0].GitHub.Title = "mutated by the caller"
			again, _ := s.List(ctx, "alice")
			same(t, again, scoped("alice", item("a", "stored")))
		}},
		{"an empty upsert is a no-op", func(t *testing.T, s worklistStore) {
			if err := s.Upsert(ctx, "alice"); err != nil {
				t.Fatal(err)
			}
			if got, _ := s.List(ctx, "alice"); len(got) != 0 {
				t.Fatalf("got %+v", got)
			}
		}},
	}
	for _, tc := range cases {
		for name, s := range worklistImpls(t) {
			t.Run(tc.name+"/"+name, func(t *testing.T) { tc.run(t, s) })
		}
	}
}

func TestWorklistPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Worklist().Upsert(ctx, "alice", item("b", "1"), item("a", "2")); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s := open(t, dir).Worklist()
	got, err := s.List(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	same(t, got, scoped("alice", item("b", "1"), item("a", "2")))
	// Order survives too: a replacement after reopen keeps its original position.
	_ = s.Upsert(ctx, "alice", item("b", "edited"), item("c", "3"))
	got, _ = s.List(ctx, "alice")
	same(t, got, scoped("alice", item("b", "edited"), item("a", "2"), item("c", "3")))
}

// An Upsert is one transaction: if any item cannot be written, none is.
func TestWorklistUpsertIsAtomic(t *testing.T) {
	db := open(t, t.TempDir())
	s := db.Worklist()
	_ = s.Upsert(ctx, "alice", item("a", "original"))
	// Make the second write of the batch fail inside the transaction.
	if _, err := db.sql.ExecContext(ctx, `
		CREATE TRIGGER reject BEFORE INSERT ON work_items WHEN NEW.id = 'poison'
		BEGIN SELECT RAISE(ABORT, 'rejected'); END`); err != nil {
		t.Fatal(err)
	}
	err := s.Upsert(ctx, "alice", item("a", "changed"), item("new", "x"), item("poison", "x"))
	if err == nil {
		t.Fatal("expected the upsert to fail")
	}
	got, _ := s.List(ctx, "alice")
	same(t, got, scoped("alice", item("a", "original")))
}

// Concurrent writers queue on the write lock (busy_timeout) instead of failing.
func TestWorklistConcurrentUpserts(t *testing.T) {
	s := open(t, t.TempDir()).Worklist()
	const writers, each = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				id := fmt.Sprintf("w%d-%d", w, i)
				if err := s.Upsert(ctx, "alice", item(id, "t")); err != nil {
					errs <- err
					return
				}
				if _, err := s.List(ctx, "alice"); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	got, _ := s.List(ctx, "alice")
	if len(got) != writers*each {
		t.Fatalf("items = %d, want %d", len(got), writers*each)
	}
	ids := make([]string, len(got))
	for i, it := range got {
		ids[i] = it.ID
	}
	sort.Strings(ids)
	for i := 1; i < len(ids); i++ {
		if ids[i] == ids[i-1] {
			t.Fatalf("duplicate id %s", ids[i])
		}
	}
}

// A document near the 8 MiB request cap (a long thread) stores and reads back.
func TestWorklistLargeDocument(t *testing.T) {
	s := open(t, t.TempDir()).Worklist()
	big := item("big", "t")
	chunk := make([]byte, 64<<10)
	for i := range chunk {
		chunk[i] = 'a' + byte(i%26)
	}
	for range 120 { // ~7.5 MiB of thread
		big.Thread = append(big.Thread, worklist.Message{Role: worklist.RoleAgent, Content: string(chunk)})
	}
	if err := s.Upsert(ctx, "alice", big); err != nil {
		t.Fatal(err)
	}
	got, err := s.List(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	same(t, got, scoped("alice", big))
}
