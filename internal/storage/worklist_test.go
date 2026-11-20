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

// A turn keeps one answer across a restart: the answered-turn rule reads the stored
// thread, so a run that writes back after gofer restarted (a retry, or the first run
// finishing late) is dropped.
func TestOneAnswerPerTurnAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	q := worklist.WorkItem{ID: "a", Thread: []worklist.Message{
		{ID: "q1", Role: worklist.RoleUser, Content: "review this", Kind: worklist.KindReviewRequest, At: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)},
	}}
	if err := db.Worklist().Upsert(ctx, "alice", q); err != nil {
		t.Fatal(err)
	}
	first := worklist.Message{ID: "r1", Content: "first", Model: "m", At: time.Date(2026, 11, 2, 9, 1, 0, 0, time.UTC)}
	if err := worklist.ApplyReply(ctx, db.Worklist(), "alice", "a", "q1", first); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s := open(t, dir).Worklist()
	second := worklist.Message{ID: "r2", Content: "second", Model: "m", At: time.Date(2026, 11, 2, 9, 2, 0, 0, time.UTC)}
	if err := worklist.ApplyReply(ctx, s, "alice", "a", "q1", second); err != nil {
		t.Fatal(err)
	}
	got, err := s.List(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	var answers []string
	for _, m := range got[0].Thread {
		if m.Role == worklist.RoleAgent {
			answers = append(answers, m.Content)
		}
	}
	if len(answers) != 1 || answers[0] != "first" {
		t.Fatalf("want one stored answer, the first, got %q", answers)
	}
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

// Update is a read-modify-write that no other write interleaves with: concurrent
// appends to one item's thread all survive. The same pattern built from List and
// Upsert loses most of them.
func TestWorklistUpdateIsAtomic(t *testing.T) {
	for name, s := range worklistImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			if err := s.Upsert(ctx, "u1", item("a", "A")); err != nil {
				t.Fatal(err)
			}
			const writers = 40
			var wg sync.WaitGroup
			errs := make(chan error, writers)
			for w := range writers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					errs <- s.Update(ctx, "u1", func(cur []worklist.WorkItem) ([]worklist.WorkItem, error) {
						for i := range cur {
							if cur[i].ID == "a" {
								cur[i].Thread = append(cur[i].Thread, worklist.Message{Role: worklist.RoleUser, Content: fmt.Sprint(w)})
								return cur[i : i+1], nil
							}
						}
						return nil, fmt.Errorf("item a missing")
					})
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			base := len(item("a", "A").Thread)
			got, _ := s.List(ctx, "u1")
			if len(got) != 1 || len(got[0].Thread) != base+writers {
				t.Fatalf("want 1 item with %d turns, got %d items, %d turns", base+writers, len(got), len(got[0].Thread))
			}
		})
	}
}

func TestWorklistUpdateSemantics(t *testing.T) {
	for name, s := range worklistImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			if err := s.Upsert(ctx, "u1", item("a", "A")); err != nil {
				t.Fatal(err)
			}
			if err := s.Upsert(ctx, "u2", item("x", "X")); err != nil {
				t.Fatal(err)
			}

			// fn sees only the owner's items.
			var seen []string
			_ = s.Update(ctx, "u1", func(cur []worklist.WorkItem) ([]worklist.WorkItem, error) {
				for _, it := range cur {
					seen = append(seen, it.ID)
				}
				return nil, nil
			})
			if len(seen) != 1 || seen[0] != "a" {
				t.Fatalf("fn should see only u1's items, saw %v", seen)
			}

			// An error writes nothing and is returned.
			boom := fmt.Errorf("boom")
			err := s.Update(ctx, "u1", func(cur []worklist.WorkItem) ([]worklist.WorkItem, error) {
				cur[0].GitHub.Title = "changed"
				return cur, boom
			})
			if err != boom {
				t.Fatalf("want fn's error, got %v", err)
			}
			got, _ := s.List(ctx, "u1")
			if got[0].GitHub.Title != "A" {
				t.Fatalf("a failed update must write nothing, title is %q", got[0].GitHub.Title)
			}

			// Returned items are added or replaced by ID, scoped to the owner.
			err = s.Update(ctx, "u1", func(cur []worklist.WorkItem) ([]worklist.WorkItem, error) {
				cur[0].GitHub.Title = "A2"
				b := item("b", "B")
				b.OwnerID = "u2" // ignored: writes are re-scoped to u1
				return []worklist.WorkItem{cur[0], b}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			got, _ = s.List(ctx, "u1")
			if len(got) != 2 || got[0].GitHub.Title != "A2" || got[1].ID != "b" || got[1].OwnerID != "u1" {
				t.Fatalf("unexpected u1 items after update: %+v", got)
			}
			other, _ := s.List(ctx, "u2")
			if len(other) != 1 || other[0].ID != "x" {
				t.Fatalf("u2's items must be untouched, got %+v", other)
			}

			// Mutating fn's copy without returning it changes nothing.
			_ = s.Update(ctx, "u1", func(cur []worklist.WorkItem) ([]worklist.WorkItem, error) {
				cur[0].Thread = append(cur[0].Thread, worklist.Message{Role: worklist.RoleUser, Content: "dropped"})
				return nil, nil
			})
			got, _ = s.List(ctx, "u1")
			if n := len(got[0].Thread); n != len(item("a", "A").Thread) || got[0].Thread[n-1].Content == "dropped" {
				t.Fatalf("an unreturned change must not be stored, thread is %+v", got[0].Thread)
			}
		})
	}
}

// Neither store shares memory with its callers: a failed update leaves nested fields
// (slices, pointers) untouched, and mutating what List returned changes nothing.
func TestWorklistNestedFieldsAreCopies(t *testing.T) {
	for name, s := range worklistImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			a := item("a", "A")
			a.Signals.Proposed = &worklist.AxisProposal{Relevance: 0.5}
			if err := s.Upsert(ctx, "u1", a); err != nil {
				t.Fatal(err)
			}
			check := func(when string) {
				t.Helper()
				got, _ := s.List(ctx, "u1")
				if got[0].Signals.Reasons[0] != worklist.ReasonReviewRequested || got[0].Signals.Proposed.Relevance != 0.5 || got[0].Thread[0].Content != a.Thread[0].Content {
					t.Fatalf("%s: stored nested fields changed: %+v %+v %+v", when, got[0].Signals.Reasons, got[0].Signals.Proposed, got[0].Thread[0])
				}
			}

			_ = s.Update(ctx, "u1", func(cur []worklist.WorkItem) ([]worklist.WorkItem, error) {
				cur[0].Signals.Reasons[0] = worklist.ReasonAuthor
				cur[0].Signals.Proposed.Relevance = 0.9
				cur[0].Thread[0].Content = "changed"
				return nil, fmt.Errorf("reject")
			})
			check("after a failed update")

			got, _ := s.List(ctx, "u1")
			got[0].Signals.Reasons[0] = worklist.ReasonAuthor
			got[0].Signals.Proposed.Relevance = 0.9
			check("after mutating a List result")

			a.Signals.Reasons[0] = worklist.ReasonAuthor // the caller's own copy, after Upsert
			check("after mutating the upserted value")
		})
	}
}

// Get and Briefs serve a run's bounded reads straight from the store. Both stores
// agree with the reference (worklist.ApplyBriefQuery): Get never crosses owners,
// Briefs returns no threads, the open filter drops retired items, and a page is the
// rows after the cursor in ID order, while an unpaged read keeps first-insertion
// order.
func TestWorklistGetAndBriefsParity(t *testing.T) {
	retired := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	seed := func() []worklist.WorkItem {
		var items []worklist.WorkItem
		for _, id := range []string{"c", "a", "e", "b", "d"} {
			it := item(id, "T"+id)
			it.Thread = []worklist.Message{{ID: "m-" + id, Role: worklist.RoleUser, Content: "body " + id}}
			if id == "b" || id == "e" {
				it.Meta.CompletedAt = retired
			}
			items = append(items, it)
		}
		return items
	}
	queries := []worklist.BriefQuery{
		{},
		{Open: true},
		{Limit: 2},
		{After: "a", Limit: 2},
		{Open: true, After: "a", Limit: 10},
		{After: "zzz", Limit: 3},
	}
	for name, s := range worklistImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			if err := s.Upsert(ctx, "u1", seed()...); err != nil {
				t.Fatal(err)
			}
			if err := s.Upsert(ctx, "u2", item("z", "other")); err != nil {
				t.Fatal(err)
			}
			for _, q := range queries {
				got, err := s.Briefs(ctx, "u1", q)
				if err != nil {
					t.Fatal(err)
				}
				want := worklist.ApplyBriefQuery(seed(), q)
				gotIDs, wantIDs := "", ""
				for _, it := range got {
					gotIDs += it.ID
					if it.Thread != nil || it.OwnerID != "u1" || it.GitHub.Title != "T"+it.ID {
						t.Fatalf("%+v: brief %+v must be the whole item without its thread", q, it)
					}
				}
				for _, it := range want {
					wantIDs += it.ID
				}
				if gotIDs != wantIDs {
					t.Fatalf("%+v: got %q, want %q", q, gotIDs, wantIDs)
				}
			}
			it, ok, err := s.Get(ctx, "u1", "a")
			if err != nil || !ok || len(it.Thread) != 1 || it.Thread[0].Content != "body a" {
				t.Fatalf("Get(u1, a) = %+v %v %v", it, ok, err)
			}
			if _, ok, err := s.Get(ctx, "u1", "z"); err != nil || ok {
				t.Fatalf("Get(u1, z) crossed owners: %v %v", ok, err)
			}
			if _, ok, err := s.Get(ctx, "u1", "nope"); err != nil || ok {
				t.Fatalf("Get(u1, nope) = %v %v", ok, err)
			}
		})
	}
}

// Owners names each owner with stored work once, in both implementations; the
// scheduled refresh reads it on every tick.
func TestWorklistOwners(t *testing.T) {
	ctx := t.Context()
	for name, s := range worklistImpls(t) {
		t.Run(name, func(t *testing.T) {
			ol, ok := s.(worklist.OwnerLister)
			if !ok {
				t.Fatal("store does not implement worklist.OwnerLister")
			}
			if got, err := ol.Owners(ctx); err != nil || len(got) != 0 {
				t.Fatalf("empty store: Owners = %v, %v", got, err)
			}
			if err := s.Upsert(ctx, "github:2", item("a", "A"), item("b", "B")); err != nil {
				t.Fatal(err)
			}
			if err := s.Upsert(ctx, "github:1", item("a", "A")); err != nil {
				t.Fatal(err)
			}
			got, err := ol.Owners(ctx)
			if err != nil {
				t.Fatal(err)
			}
			sort.Strings(got)
			if fmt.Sprint(got) != "[github:1 github:2]" {
				t.Fatalf("Owners = %v, want [github:1 github:2]", got)
			}
		})
	}
}
