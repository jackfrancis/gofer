package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// now is the fake clock every test's Snapshotter runs on; the fake API server stamps
// new snapshots with it too.
var now = time.Date(2026, 11, 17, 19, 30, 5, 0, time.UTC)

func ago(d time.Duration) time.Time { return now.Add(-d) }

// contentRef is a VolumeSnapshotContent's back-reference to its snapshot.
type contentRef struct{ name, namespace, uid string }

// fakeAPI is a tiny stand-in for the API server's snapshot endpoints. It applies the
// label selector the way the API server would, and records every call.
type fakeAPI struct {
	mu        sync.Mutex
	snaps     map[string]map[string]any // name -> object
	contents  map[string]string         // content name -> deletionPolicy
	refs      map[string]contentRef     // content name -> its snapshot reference
	created   map[string]bool           // snapshots created through POST
	calls     []string
	token     string
	readyAt   int       // GETs of a created snapshot before it reports ready; -1 never
	failWith  string    // error a created snapshot reports while not ready
	errorAt   time.Time // the time on that error, if any
	unbound   bool      // a created snapshot's status never names its content
	reject    string    // refuse every create with this message
	failGetOf string    // answer GETs of this content with a 500
	onGet     func(name string, obj map[string]any, n int)
	onPatch   func(content string) // runs as a content's policy is changed
	// onContentGet runs after a content has been read, before anything else.
	onContentGet func(content string)
	// fault decides how a PATCH or DELETE to path fails: "before" answers 500 without
	// applying it, "after" applies it and answers 500 (a lost response).
	fault     func(method, path string) string
	contentRV map[string]int // content name -> resourceVersion
	getsByKey map[string]int
}

// contentUID is the UID the fake gives a content.
func contentUID(name string) string { return "cuid-" + name }

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		snaps: map[string]map[string]any{}, contents: map[string]string{}, refs: map[string]contentRef{},
		created: map[string]bool{}, token: "tok", getsByKey: map[string]int{}, contentRV: map[string]int{},
	}
}

func (f *fakeAPI) add(name string, created time.Time, ready bool, labels map[string]string) {
	f.snaps[name] = map[string]any{
		"metadata": map[string]any{"name": name, "uid": "uid-" + name, "resourceVersion": "1", "labels": labels, "creationTimestamp": created.Format(time.RFC3339)},
		"status":   map[string]any{"readyToUse": ready, "boundVolumeSnapshotContentName": "content-" + name, "restoreSize": "1Gi"},
	}
	f.contents["content-"+name] = "Retain"
	f.refs["content-"+name] = contentRef{name, "gofer", "uid-" + name}
}

// bump records a change to an object, as the API server does with resourceVersion.
func bump(obj map[string]any) {
	meta := obj["metadata"].(map[string]any)
	meta["resourceVersion"] = meta["resourceVersion"].(string) + "+"
}

// later makes s's clock jump past the default grace period once Take has named the
// snapshot, as if the wait had taken that long.
func later(s *Snapshotter) {
	calls := 0
	s.Now = func() time.Time {
		calls++
		if calls == 1 {
			return now
		}
		return now.Add(DefaultGrace + time.Minute)
	}
}

// addFailed seeds a snapshot that reports an error and is not ready.
func (f *fakeAPI) addFailed(name string, created time.Time, labels map[string]string) {
	f.add(name, created, false, labels)
	f.status(name)["error"] = map[string]any{"message": "boom"}
}

// addUnbound seeds a failed snapshot whose status does not name its content, and a
// dynamic content for it (snapcontent-<uid>) referring back to ref.
func (f *fakeAPI) addUnbound(name string, created time.Time, ref contentRef) {
	f.addFailed(name, created, managed())
	delete(f.status(name), "boundVolumeSnapshotContentName")
	delete(f.contents, "content-"+name)
	delete(f.refs, "content-"+name)
	f.contents["snapcontent-uid-"+name] = "Retain"
	f.refs["snapcontent-uid-"+name] = ref
}

func (f *fakeAPI) status(name string) map[string]any { return f.snaps[name]["status"].(map[string]any) }

func (f *fakeAPI) terminate(obj map[string]any) {
	obj["metadata"].(map[string]any)["deletionTimestamp"] = now.Format(time.RFC3339)
}

func (f *fakeAPI) deletes() []string {
	var out []string
	for _, c := range f.calls {
		if name, ok := strings.CutPrefix(c, "DELETE "+snapPrefix+"/"); ok {
			out = append(out, name)
		}
	}
	return out
}

func managed() map[string]string {
	return map[string]string{LabelManagedBy: ManagedBy, LabelClaim: "data-gofer-0"}
}

const (
	snapPrefix    = "/apis/snapshot.storage.k8s.io/v1/namespaces/gofer/volumesnapshots"
	contentPrefix = "/apis/snapshot.storage.k8s.io/v1/volumesnapshotcontents/"
)

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	fault := ""
	if f.fault != nil && (r.Method == "PATCH" || r.Method == "DELETE") {
		fault = f.fault(r.Method, r.URL.Path)
	}
	if fault == "before" {
		http.Error(w, `{"message":"internal error"}`, http.StatusInternalServerError)
		return
	}
	if fault == "after" {
		w = &lostResponse{ResponseWriter: w}
		defer func() {
			http.Error(w.(*lostResponse).ResponseWriter, `{"message":"timeout"}`, http.StatusInternalServerError)
		}()
	}
	switch {
	case r.Method == "POST" && r.URL.Path == snapPrefix:
		if f.reject != "" {
			http.Error(w, `{"message":"`+f.reject+`"}`, http.StatusForbidden)
			return
		}
		var obj map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &obj)
		meta := obj["metadata"].(map[string]any)
		name := meta["name"].(string)
		meta["creationTimestamp"] = now.Format(time.RFC3339) // set by the API server
		meta["uid"] = "uid-" + name
		meta["resourceVersion"] = "1"
		f.snaps[name] = obj
		f.created[name] = true
		// The controller binds a content with the class's deletionPolicy.
		if f.unbound {
			f.contents["snapcontent-uid-"+name] = "Retain"
			f.refs["snapcontent-uid-"+name] = contentRef{name, "gofer", "uid-" + name}
		} else {
			f.contents["content-"+name] = "Retain"
			f.refs["content-"+name] = contentRef{name, "gofer", "uid-" + name}
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(body)
	case r.Method == "GET" && r.URL.Path == snapPrefix:
		want := map[string]string{}
		for _, kv := range strings.Split(r.URL.Query().Get("labelSelector"), ",") {
			k, v, _ := strings.Cut(kv, "=")
			want[k] = v
		}
		var items []map[string]any
		for _, obj := range f.snaps {
			match := true
			for k, v := range want {
				if labelOf(obj, k) != v {
					match = false
				}
			}
			if match {
				items = append(items, obj)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, snapPrefix+"/"):
		name := strings.TrimPrefix(r.URL.Path, snapPrefix+"/")
		obj, ok := f.snaps[name]
		if !ok {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		f.getsByKey[name]++
		if f.created[name] {
			status := map[string]any{"readyToUse": f.readyAt >= 0 && f.getsByKey[name] > f.readyAt, "restoreSize": "1Gi"}
			if !f.unbound {
				status["boundVolumeSnapshotContentName"] = "content-" + name
			}
			if f.failWith != "" && !status["readyToUse"].(bool) {
				e := map[string]any{"message": f.failWith}
				if !f.errorAt.IsZero() {
					e["time"] = f.errorAt.Format(time.RFC3339)
				}
				status["error"] = e
			}
			obj["status"] = status
		}
		if f.onGet != nil {
			f.onGet(name, obj, f.getsByKey[name])
		}
		_ = json.NewEncoder(w).Encode(obj)
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, snapPrefix+"/"):
		name := strings.TrimPrefix(r.URL.Path, snapPrefix+"/")
		obj, ok := f.snaps[name]
		if !ok {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		var opts struct {
			Preconditions struct {
				UID             string `json:"uid"`
				ResourceVersion string `json:"resourceVersion"`
			} `json:"preconditions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&opts)
		meta := obj["metadata"].(map[string]any)
		if pc := opts.Preconditions; (pc.UID != "" && pc.UID != meta["uid"]) || (pc.ResourceVersion != "" && pc.ResourceVersion != meta["resourceVersion"]) {
			http.Error(w, `{"message":"precondition failed"}`, http.StatusConflict)
			return
		}
		delete(f.snaps, name)
		w.WriteHeader(http.StatusOK)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, contentPrefix):
		name := strings.TrimPrefix(r.URL.Path, contentPrefix)
		if name == f.failGetOf {
			http.Error(w, `{"message":"etcd unavailable"}`, http.StatusInternalServerError)
			return
		}
		_, ok := f.refs[name]
		if !ok {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(f.content(name))
		if f.onContentGet != nil {
			f.onContentGet(name)
		}
	case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, contentPrefix):
		if r.Header.Get("Content-Type") != "application/merge-patch+json" {
			http.Error(w, `{"message":"bad patch type"}`, http.StatusUnsupportedMediaType)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, contentPrefix)
		if _, ok := f.refs[name]; !ok {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		var patch struct {
			Metadata struct {
				UID             string `json:"uid"`
				ResourceVersion string `json:"resourceVersion"`
			} `json:"metadata"`
			Spec struct {
				DeletionPolicy string `json:"deletionPolicy"`
			} `json:"spec"`
		}
		_ = json.NewDecoder(r.Body).Decode(&patch)
		if m := patch.Metadata; (m.UID != "" && m.UID != contentUID(name)) || (m.ResourceVersion != "" && m.ResourceVersion != strconv.Itoa(f.contentRV[name])) {
			http.Error(w, `{"message":"the object has been modified"}`, http.StatusConflict)
			return
		}
		f.contents[name] = patch.Spec.DeletionPolicy
		f.contentRV[name]++
		if f.onPatch != nil {
			f.onPatch(name)
		}
		_ = json.NewEncoder(w).Encode(f.content(name))
	default:
		http.Error(w, `{"message":"unexpected"}`, http.StatusTeapot)
	}
}

// content is a content object as the fake serves it.
func (f *fakeAPI) content(name string) map[string]any {
	ref := f.refs[name]
	return map[string]any{
		"metadata": map[string]any{"name": name, "uid": contentUID(name), "resourceVersion": strconv.Itoa(f.contentRV[name])},
		"spec": map[string]any{
			"deletionPolicy":    f.contents[name],
			"volumeSnapshotRef": map[string]any{"name": ref.name, "namespace": ref.namespace, "uid": ref.uid},
		},
	}
}

// lostResponse swallows a response, so the request is applied but its answer lost.
type lostResponse struct{ http.ResponseWriter }

func (l *lostResponse) WriteHeader(int)             {}
func (l *lostResponse) Write(b []byte) (int, error) { return len(b), nil }

func newSnapshotter(t *testing.T, f *fakeAPI) *Snapshotter {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &Snapshotter{
		Kube:      NewKube(srv.URL, func() (string, error) { return "tok", nil }, srv.Client()),
		Namespace: "gofer",
		Claim:     "data-gofer-0",
		Class:     "snaps",
		Keep:      2,
		Poll:      time.Millisecond,
		Now:       func() time.Time { return now },
	}
}

// newName is the name Take gives the snapshot it creates at now.
const newName = "data-gofer-0-20261117-193005"

func TestTakeCreatesALabeledSnapshot(t *testing.T) {
	f := newFakeAPI()
	s := newSnapshotter(t, f)
	name, err := s.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if name != newName {
		t.Fatalf("unexpected name %q", name)
	}
	obj := f.snaps[name]
	spec := obj["spec"].(map[string]any)
	if spec["volumeSnapshotClassName"] != "snaps" || spec["source"].(map[string]any)["persistentVolumeClaimName"] != "data-gofer-0" {
		t.Fatalf("unexpected spec %+v", spec)
	}
	labels := obj["metadata"].(map[string]any)["labels"].(map[string]any)
	if labels[LabelManagedBy] != ManagedBy || labels[LabelClaim] != "data-gofer-0" {
		t.Fatalf("unexpected labels %+v", labels)
	}
}

func TestTakeNeedsAClass(t *testing.T) {
	s := newSnapshotter(t, newFakeAPI())
	s.Class = ""
	if _, err := s.Take(context.Background()); err == nil {
		t.Fatal("want an error without a snapshot class")
	}
}

func TestWait(t *testing.T) {
	wait := func(t *testing.T, f *fakeAPI, d time.Duration) (string, error) {
		s := newSnapshotter(t, f)
		name, _ := s.Take(context.Background())
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		return s.Wait(ctx, name)
	}
	t.Run("ready", func(t *testing.T) {
		f := newFakeAPI()
		f.readyAt = 2
		if size, err := wait(t, f, time.Second); err != nil || size != "1Gi" {
			t.Fatalf("want ready with a size, got %q %v", size, err)
		}
	})
	// The controller retries after an error, so an error does not end the wait.
	t.Run("recovers from an error", func(t *testing.T) {
		f := newFakeAPI()
		f.readyAt, f.failWith = 1, "transient"
		if size, err := wait(t, f, time.Second); err != nil || size != "1Gi" {
			t.Fatalf("want the retried snapshot ready, got %q %v", size, err)
		}
	})
	t.Run("ready despite a stale error", func(t *testing.T) {
		f := newFakeAPI()
		f.readyAt = 0
		f.onGet = func(_ string, obj map[string]any, _ int) {
			obj["status"].(map[string]any)["error"] = map[string]any{"message": "old"}
		}
		if size, err := wait(t, f, time.Second); err != nil || size != "1Gi" {
			t.Fatalf("a snapshot ready to use is usable, got %q %v", size, err)
		}
	})
	t.Run("still failing", func(t *testing.T) {
		f := newFakeAPI()
		f.readyAt, f.failWith = -1, "quota exceeded"
		_, err := wait(t, f, 20*time.Millisecond)
		var failing *FailedError
		if !errors.As(err, &failing) || failing.Message != "quota exceeded" || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("want a FailedError wrapping the deadline, got %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		f := newFakeAPI()
		f.readyAt = -1
		_, err := wait(t, f, 20*time.Millisecond)
		var failing *FailedError
		if errors.As(err, &failing) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("want a plain timeout, got %v", err)
		}
	})
}

func TestPrune(t *testing.T) {
	f := newFakeAPI()
	day := func(n int) time.Time { return ago(time.Duration(7-n) * 24 * time.Hour) }
	f.add("in-progress", day(6), false, managed())      // newest, not ready yet: keep
	f.add("good-5", day(5), true, managed())            // usable #1: keep
	f.add("pending-4", day(4), false, managed())        // between keepers: keep
	f.add("good-3", day(3), true, managed())            // usable #2: keep
	f.add("good-2", day(2), true, managed())            // beyond Keep=2: delete
	f.add("pending-1", day(1), false, managed())        // beyond: delete
	f.add("by-hand", day(0), true, map[string]string{}) // not managed: never touched
	f.add("other-claim", day(0), true, map[string]string{LabelManagedBy: ManagedBy, LabelClaim: "other"})
	s := newSnapshotter(t, f)

	deleted, err := s.Prune(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(deleted)
	if !slices.Equal(deleted, []string{"good-2", "pending-1"}) {
		t.Fatalf("deleted %v", deleted)
	}
	for _, name := range []string{"in-progress", "good-5", "pending-4", "good-3", "by-hand", "other-claim"} {
		if _, ok := f.snaps[name]; !ok {
			t.Errorf("%s must be kept", name)
		}
	}
	// Each pruned snapshot's stored content is switched to Delete first, so the
	// storage goes with it; kept ones stay Retain.
	if f.contents["content-good-2"] != "Delete" || f.contents["content-pending-1"] != "Delete" {
		t.Fatalf("pruned contents must be set to Delete, got %v", f.contents)
	}
	if f.contents["content-good-3"] != "Retain" {
		t.Fatal("a kept snapshot's content must stay Retain")
	}
	for i, call := range f.calls {
		if strings.HasPrefix(call, "DELETE") && !strings.HasPrefix(f.calls[i-1], "PATCH") {
			t.Fatalf("every delete must follow its content patch: %v", f.calls)
		}
	}
}

// Failed snapshots go wherever they sit; in-progress ones inside the window, ones
// still within their grace period, and usable ones up to Keep stay.
func TestPruneDeletesFailedAnywhere(t *testing.T) {
	f := newFakeAPI()
	day := func(n int) time.Time { return ago(time.Duration(9-n) * 24 * time.Hour) }
	f.addFailed("failed-8", day(8), managed())           // failed, newest: delete
	f.addFailed("retrying", ago(time.Minute), managed()) // erroring, within grace: keep
	f.add("in-progress", day(7), false, managed())       // not ready, no error: keep
	f.add("stale-error", day(6), true, managed())        // ready with an old error: usable #1, keep
	f.status("stale-error")["error"] = map[string]any{"message": "transient"}
	f.addFailed("failed-5", day(5), managed())          // between keepers: delete
	f.add("good-4", day(4), true, managed())            // usable #2: keep
	f.add("good-3", day(3), true, managed())            // beyond Keep=2: delete
	f.addFailed("failed-2", day(2), managed())          // beyond: delete
	f.addFailed("by-hand", day(1), map[string]string{}) // not managed: never touched
	s := newSnapshotter(t, f)

	deleted, err := s.Prune(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(deleted)
	if !slices.Equal(deleted, []string{"failed-2", "failed-5", "failed-8", "good-3"}) {
		t.Fatalf("deleted %v", deleted)
	}
	for _, name := range []string{"retrying", "in-progress", "stale-error", "good-4", "by-hand"} {
		if _, ok := f.snaps[name]; !ok {
			t.Errorf("%s must be kept", name)
		}
	}
	if f.contents["content-failed-5"] != "Delete" {
		t.Fatal("a failed snapshot's stored content must go with it")
	}
}

// The grace period is the Snapshotter's to set.
func TestPruneGrace(t *testing.T) {
	f := newFakeAPI()
	f.addFailed("erroring", ago(30*time.Minute), managed())
	f.add("good", ago(time.Hour), true, managed())
	s := newSnapshotter(t, f)
	s.Grace = time.Hour
	if deleted, err := s.Prune(context.Background()); err != nil || len(deleted) != 0 {
		t.Fatalf("an error younger than the grace period must be kept, got %v %v", deleted, err)
	}
	s.Grace = 20 * time.Minute
	if deleted, err := s.Prune(context.Background()); err != nil || !slices.Equal(deleted, []string{"erroring"}) {
		t.Fatalf("want the erroring snapshot deleted past its grace, got %v %v", deleted, err)
	}
}

// The grace period runs from when the error appeared, not from the snapshot's
// creation: a slow snapshot that only just started erroring gets the full period.
func TestPruneTimesTheErrorNotTheSnapshot(t *testing.T) {
	f := newFakeAPI()
	f.addFailed("late-error", ago(6*time.Hour), managed())
	f.status("late-error")["error"].(map[string]any)["time"] = ago(time.Second).Format(time.RFC3339)
	f.addFailed("old-error", ago(6*time.Hour), managed())
	f.status("old-error")["error"].(map[string]any)["time"] = ago(DefaultGrace + time.Minute).Format(time.RFC3339)
	f.add("good", ago(7*time.Hour), true, managed())
	s := newSnapshotter(t, f)
	deleted, err := s.Prune(context.Background())
	if err != nil || !slices.Equal(deleted, []string{"old-error"}) {
		t.Fatalf("want only the error older than the grace period deleted, got %v %v", deleted, err)
	}
}

// An erroring snapshot older than MaxErrorAge has failed however fresh its error:
// a driver whose message changes on each attempt restarts the error's clock.
func TestPruneMaxErrorAge(t *testing.T) {
	f := newFakeAPI()
	f.addFailed("churning", ago(MaxErrorAge+time.Hour), managed())
	f.status("churning")["error"].(map[string]any)["time"] = ago(time.Second).Format(time.RFC3339)
	f.add("good", ago(MaxErrorAge+2*time.Hour), true, managed())
	s := newSnapshotter(t, f)
	if deleted, err := s.pruneFailed(context.Background()); err != nil || !slices.Equal(deleted, []string{"churning"}) {
		t.Fatalf("want the churning snapshot deleted, got %v %v", deleted, err)
	}
}

// A snapshot whose status names another snapshot's content (SnapshotMisbound) is
// deleted without touching that content: it is another backup's storage.
func TestDeleteChecksStatusNamedContent(t *testing.T) {
	f := newFakeAPI()
	f.add("good", ago(2*time.Hour), true, managed())
	f.addFailed("misbound", ago(time.Hour), managed())
	delete(f.contents, "content-misbound")
	delete(f.refs, "content-misbound")
	f.status("misbound")["boundVolumeSnapshotContentName"] = "content-good"
	s := newSnapshotter(t, f)
	if deleted, err := s.pruneFailed(context.Background()); err != nil || !slices.Equal(deleted, []string{"misbound"}) {
		t.Fatalf("want the misbound snapshot deleted, got %v %v", deleted, err)
	}
	if f.contents["content-good"] != "Retain" {
		t.Fatal("another snapshot's content must keep its policy")
	}
	if _, ok := f.snaps["good"]; !ok {
		t.Fatal("the healthy backup must stay")
	}
}

// The delete is conditional on the snapshot as checked. If it changes in between
// (here it becomes ready while its content's policy is switched), it is not deleted
// and its content gets its policy back.
func TestDeleteIsConditionalOnTheVersionChecked(t *testing.T) {
	f := newFakeAPI()
	f.addFailed("flipping", ago(time.Hour), managed())
	f.onPatch = func(content string) {
		if content == "content-flipping" && f.contents[content] == "Delete" {
			f.snaps["flipping"]["status"] = map[string]any{"readyToUse": true, "boundVolumeSnapshotContentName": "content-flipping"}
			bump(f.snaps["flipping"])
		}
	}
	s := newSnapshotter(t, f)
	deleted, err := s.pruneFailed(context.Background())
	if err != nil || len(deleted) != 0 {
		t.Fatalf("want nothing deleted, got %v %v", deleted, err)
	}
	if _, ok := f.snaps["flipping"]; !ok {
		t.Fatal("a snapshot that became ready must not be deleted")
	}
	if f.contents["content-flipping"] != "Retain" {
		t.Fatalf("its content must get Retain back, have %q", f.contents["content-flipping"])
	}
}

// A content's policy is never changed while its snapshot is going. If another
// deletion gets there between the switch and the delete (another backup job, which
// found the content already at Delete, or anyone else), the content keeps the policy
// that deletion found, so its storage is not stranded.
func TestDeleteLeavesPolicyWhenBeatenToIt(t *testing.T) {
	for name, beat := range map[string]func(f *fakeAPI){
		"gone":        func(f *fakeAPI) { delete(f.snaps, "raced") },
		"terminating": func(f *fakeAPI) { f.terminate(f.snaps["raced"]); bump(f.snaps["raced"]) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeAPI()
			f.addFailed("raced", ago(time.Hour), managed())
			f.onPatch = func(content string) {
				if f.contents[content] == "Delete" {
					beat(f)
				}
			}
			s := newSnapshotter(t, f)
			if deleted, err := s.pruneFailed(context.Background()); err != nil || len(deleted) != 0 {
				t.Fatalf("want nothing reported deleted, got %v %v", deleted, err)
			}
			if f.contents["content-raced"] != "Delete" {
				t.Fatalf("the content must keep the policy the deletion found, have %q", f.contents["content-raced"])
			}
		})
	}
}

// The same holds when the switch's answer is lost and the snapshot is meanwhile being
// deleted: the undo leaves the content alone.
func TestUndoLeavesAGoingSnapshotsContent(t *testing.T) {
	f := newFakeAPI()
	f.addFailed("raced", ago(time.Hour), managed())
	f.fault = func(method, _ string) string {
		if method == "PATCH" {
			return "after"
		}
		return ""
	}
	f.onPatch = func(string) { f.terminate(f.snaps["raced"]) }
	s := newSnapshotter(t, f)
	_, _ = s.pruneFailed(context.Background())
	if f.contents["content-raced"] != "Delete" {
		t.Fatalf("a going snapshot's content must keep its policy, have %q", f.contents["content-raced"])
	}
}

// A deletion that has made its first write finishes even if the job is stopped then,
// rather than leaving the snapshot behind with its content at Delete.
func TestDeleteFinishesAfterCancellation(t *testing.T) {
	f := newFakeAPI()
	f.addFailed("failed", ago(time.Hour), managed())
	f.add("good", ago(2*time.Hour), true, managed())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.onPatch = func(string) { cancel() } // SIGTERM lands just after the switch
	s := newSnapshotter(t, f)
	deleted, _ := s.pruneFailed(ctx)
	if !slices.Equal(deleted, []string{"failed"}) {
		t.Fatalf("the started deletion must finish, deleted %v", deleted)
	}
	if _, ok := f.snaps["failed"]; ok {
		t.Fatal("the snapshot must be gone, not left with its content at Delete")
	}
	if _, ok := f.snaps["good"]; !ok || f.contents["content-good"] != "Retain" {
		t.Fatal("nothing else may be touched after the job is stopped")
	}
}

// A taken snapshot whose content has gone, or belongs to another snapshot, has failed
// once its error outlasts the grace period: it was ready once, and has no storage of
// its own left. One whose content cannot be read is kept.
func TestPruneTakenWithoutItsContent(t *testing.T) {
	taken := func(f *fakeAPI, name string) {
		f.addFailed(name, ago(48*time.Hour), managed())
		f.status(name)["creationTime"] = ago(48 * time.Hour).Format(time.RFC3339)
		f.status(name)["error"].(map[string]any)["time"] = ago(47 * time.Hour).Format(time.RFC3339)
	}
	t.Run("content missing", func(t *testing.T) {
		f := newFakeAPI()
		taken(f, "orphan")
		delete(f.refs, "content-orphan")
		delete(f.contents, "content-orphan")
		s := newSnapshotter(t, f)
		if deleted, err := s.pruneFailed(context.Background()); err != nil || !slices.Equal(deleted, []string{"orphan"}) {
			t.Fatalf("want it deleted, got %v %v", deleted, err)
		}
	})
	t.Run("content belongs to another", func(t *testing.T) {
		f := newFakeAPI()
		f.add("good", ago(49*time.Hour), true, managed())
		taken(f, "misbound")
		f.status("misbound")["boundVolumeSnapshotContentName"] = "content-good"
		s := newSnapshotter(t, f)
		if deleted, err := s.pruneFailed(context.Background()); err != nil || !slices.Equal(deleted, []string{"misbound"}) {
			t.Fatalf("want it deleted, got %v %v", deleted, err)
		}
		if f.contents["content-good"] != "Retain" {
			t.Fatal("the other snapshot's content must not be touched")
		}
	})
	t.Run("content unreadable", func(t *testing.T) {
		f := newFakeAPI()
		taken(f, "unsure")
		f.failGetOf = "content-unsure"
		s := newSnapshotter(t, f)
		if deleted, _ := s.pruneFailed(context.Background()); len(deleted) != 0 {
			t.Fatalf("want it kept, deleted %v", deleted)
		}
	})
	t.Run("content still there", func(t *testing.T) {
		f := newFakeAPI()
		taken(f, "copying")
		s := newSnapshotter(t, f)
		if deleted, err := s.pruneFailed(context.Background()); err != nil || len(deleted) != 0 {
			t.Fatalf("want it kept, got %v %v", deleted, err)
		}
	})
	// The case that matters most: it fills the last quota slot, so no new snapshot can
	// be created and only the failed-only sweep runs.
	t.Run("quota full", func(t *testing.T) {
		f := newFakeAPI()
		f.reject = "exceeded quota"
		taken(f, "orphan")
		delete(f.refs, "content-orphan")
		delete(f.contents, "content-orphan")
		s := newSnapshotter(t, f)
		if err := s.Run(context.Background(), time.Second); err == nil {
			t.Fatal("want the create error")
		}
		if _, ok := f.snaps["orphan"]; ok {
			t.Fatal("the orphaned snapshot must be deleted even when no snapshot can be created")
		}
	})
}

// A content already set to Delete is not patched, and so not set back either.
func TestDeleteLeavesADeletePolicyAlone(t *testing.T) {
	f := newFakeAPI()
	f.addFailed("failed", ago(time.Hour), managed())
	f.contents["content-failed"] = "Delete"
	s := newSnapshotter(t, f)
	if deleted, err := s.pruneFailed(context.Background()); err != nil || !slices.Equal(deleted, []string{"failed"}) {
		t.Fatalf("want it deleted, got %v %v", deleted, err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "PATCH") {
			t.Fatalf("unexpected %s", c)
		}
	}
}

// A snapshot the storage system has already taken (status.creationTime set) is still
// copying, whatever earlier error it shows: it is slow, not failed.
func TestPruneKeepsATakenSnapshot(t *testing.T) {
	f := newFakeAPI()
	taken := func(name string, created time.Time) {
		f.addFailed(name, created, managed())
		f.status(name)["error"].(map[string]any)["time"] = created.Add(time.Minute).Format(time.RFC3339)
		f.status(name)["creationTime"] = created.Add(11 * time.Minute).Format(time.RFC3339)
	}
	taken("copying", ago(time.Hour))
	taken("copying-long", ago(MaxErrorAge+time.Hour))
	f.add("good", ago(MaxErrorAge+2*time.Hour), true, managed())
	s := newSnapshotter(t, f)
	if deleted, err := s.pruneFailed(context.Background()); err != nil || len(deleted) != 0 {
		t.Fatalf("a taken snapshot must not count as failed, deleted %v %v", deleted, err)
	}
	// Like any slow snapshot, it still goes once it is beyond the newest Keep usable.
	taken("copying-old", ago(MaxErrorAge+3*time.Hour))
	s.Keep = 1
	if deleted, err := s.Prune(context.Background()); err != nil || !slices.Equal(deleted, []string{"copying-old"}) {
		t.Fatalf("want only the one beyond the window pruned, got %v %v", deleted, err)
	}
}

// Wait does not report a taken snapshot as failing.
func TestWaitTakenIsSlowNotFailing(t *testing.T) {
	f := newFakeAPI()
	f.readyAt, f.failWith = -1, "earlier attempt failed"
	f.onGet = func(_ string, obj map[string]any, _ int) {
		obj["status"].(map[string]any)["creationTime"] = now.Format(time.RFC3339)
	}
	s := newSnapshotter(t, f)
	name, _ := s.Take(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := s.Wait(ctx, name)
	var failing *FailedError
	if errors.As(err, &failing) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want a plain timeout, got %v", err)
	}
}

// The snapshot deleted is the one listed: if its labels are removed, or it is
// replaced under the same name, between the list and the delete, it is left alone.
func TestDeleteChecksTheListedIdentity(t *testing.T) {
	for name, change := range map[string]func(obj map[string]any){
		"labels removed": func(obj map[string]any) { obj["metadata"].(map[string]any)["labels"] = map[string]string{} },
		"replaced":       func(obj map[string]any) { obj["metadata"].(map[string]any)["uid"] = "uid-a-new-object" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeAPI()
			f.addFailed("failed", ago(time.Hour), managed())
			f.onGet = func(_ string, obj map[string]any, _ int) { change(obj) }
			s := newSnapshotter(t, f)
			if deleted, err := s.pruneFailed(context.Background()); err != nil || len(deleted) != 0 {
				t.Fatalf("want nothing deleted, got %v %v", deleted, err)
			}
			if f.contents["content-failed"] != "Retain" || len(f.deletes()) != 0 {
				t.Fatalf("nothing may be touched: policy %q, deletes %v", f.contents["content-failed"], f.deletes())
			}
		})
	}
}

// The content's switch to Delete is conditional on the content as checked: if it is
// rebound to another snapshot in between, it is not switched.
func TestDeleteSwitchesOnlyTheContentChecked(t *testing.T) {
	f := newFakeAPI()
	f.addFailed("failed", ago(time.Hour), managed())
	f.onContentGet = func(name string) {
		f.refs[name] = contentRef{"good", "gofer", "uid-good"}
		f.contentRV[name]++
	}
	s := newSnapshotter(t, f)
	if deleted, err := s.pruneFailed(context.Background()); err != nil || len(deleted) != 0 {
		t.Fatalf("want nothing deleted, got %v %v", deleted, err)
	}
	if f.contents["content-failed"] != "Retain" {
		t.Fatal("a content that changed since it was checked must not be switched")
	}
}

// Writes whose outcome is unknown are checked before anything is undone.
func TestDeleteUncertainWrites(t *testing.T) {
	t.Run("switch applied, answer lost", func(t *testing.T) {
		f := newFakeAPI()
		f.addFailed("failed", ago(time.Hour), managed())
		patches := 0
		f.fault = func(method, _ string) string {
			if method == "PATCH" {
				if patches++; patches == 1 {
					return "after"
				}
			}
			return ""
		}
		s := newSnapshotter(t, f)
		if _, err := s.pruneFailed(context.Background()); err == nil {
			t.Fatal("want the failed switch reported")
		}
		if _, ok := f.snaps["failed"]; !ok || f.contents["content-failed"] != "Retain" {
			t.Fatalf("the snapshot must stay with its policy back to Retain, have %q", f.contents["content-failed"])
		}
	})
	t.Run("delete applied, answer lost", func(t *testing.T) {
		f := newFakeAPI()
		f.addFailed("failed", ago(time.Hour), managed())
		f.fault = func(method, _ string) string {
			if method == "DELETE" {
				return "after"
			}
			return ""
		}
		s := newSnapshotter(t, f)
		if deleted, err := s.pruneFailed(context.Background()); err != nil || !slices.Equal(deleted, []string{"failed"}) {
			t.Fatalf("want it counted as deleted, got %v %v", deleted, err)
		}
		if f.contents["content-failed"] != "Delete" {
			t.Fatal("a snapshot that went must keep Delete, or its storage is stranded")
		}
	})
	t.Run("delete not applied", func(t *testing.T) {
		f := newFakeAPI()
		f.addFailed("failed", ago(time.Hour), managed())
		f.fault = func(method, _ string) string {
			if method == "DELETE" {
				return "before"
			}
			return ""
		}
		s := newSnapshotter(t, f)
		if _, err := s.pruneFailed(context.Background()); err == nil {
			t.Fatal("want the failed delete reported")
		}
		if _, ok := f.snaps["failed"]; !ok || f.contents["content-failed"] != "Retain" {
			t.Fatalf("the snapshot must stay with its policy back to Retain, have %q", f.contents["content-failed"])
		}
	})
	t.Run("undo answer lost too", func(t *testing.T) {
		f := newFakeAPI()
		f.addFailed("failed", ago(time.Hour), managed())
		f.fault = func(method, _ string) string {
			switch method {
			case "DELETE":
				return "before"
			case "PATCH":
				return "after" // every switch, the undo included, loses its answer
			}
			return ""
		}
		s := newSnapshotter(t, f)
		_, err := s.pruneFailed(context.Background())
		if f.contents["content-failed"] != "Retain" {
			t.Fatalf("the undo must be confirmed by reading back, have %q", f.contents["content-failed"])
		}
		if err == nil || strings.Contains(err.Error(), "by hand") {
			t.Fatalf("want the failed delete reported, without a false alarm about the policy, got %v", err)
		}
	})
}

// A snapshot that recovers between the list and the delete is not deleted.
func TestPruneRechecksBeforeDeleting(t *testing.T) {
	f := newFakeAPI()
	f.addFailed("recovered", ago(time.Hour), managed())
	f.add("good", ago(2*time.Hour), true, managed())
	f.onGet = func(name string, obj map[string]any, _ int) {
		if name == "recovered" {
			obj["status"] = map[string]any{"readyToUse": true, "boundVolumeSnapshotContentName": "content-recovered"}
		}
	}
	s := newSnapshotter(t, f)
	if deleted, err := s.Prune(context.Background()); err != nil || len(deleted) != 0 {
		t.Fatalf("want nothing deleted, got %v %v", deleted, err)
	}
	if f.contents["content-recovered"] != "Retain" {
		t.Fatal("a recovered snapshot's content must stay Retain")
	}
}

// Deleting failed snapshots never lowers the number of usable ones below Keep.
func TestPruneFailedNeverCostsAUsableOne(t *testing.T) {
	f := newFakeAPI()
	for i := range 3 {
		f.addFailed("failed-"+string(rune('a'+i)), ago(time.Duration(10-2*i)*time.Hour), managed())
		f.add("good-"+string(rune('a'+i)), ago(time.Duration(9-2*i)*time.Hour), true, managed())
	}
	s := newSnapshotter(t, f)
	s.Keep = 3
	deleted, err := s.Prune(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(deleted)
	if !slices.Equal(deleted, []string{"failed-a", "failed-b", "failed-c"}) {
		t.Fatalf("deleted %v", deleted)
	}
	if len(f.snaps) != 3 {
		t.Fatalf("all three usable snapshots must stay, have %v", keys(f.snaps))
	}
}

// pruneFailed touches only failed snapshots, never usable, in-progress or still
// retrying ones.
func TestPruneFailedOnly(t *testing.T) {
	f := newFakeAPI()
	for i := range 4 {
		f.add("good-"+string(rune('a'+i)), ago(time.Duration(i+1)*time.Hour), true, managed())
	}
	f.add("old-in-progress", ago(9*time.Hour), false, managed())
	f.addFailed("retrying", ago(time.Minute), managed())
	f.addFailed("failed", ago(10*time.Hour), managed())
	s := newSnapshotter(t, f)
	s.Keep = 1
	deleted, err := s.pruneFailed(context.Background())
	if err != nil || !slices.Equal(deleted, []string{"failed"}) {
		t.Fatalf("want only the failed one deleted, got %v %v", deleted, err)
	}
	if len(f.snaps) != 6 {
		t.Fatalf("have %v", keys(f.snaps))
	}
}

// A snapshot already being deleted is neither counted as a backup nor deleted again.
func TestPruneSkipsTerminating(t *testing.T) {
	f := newFakeAPI()
	f.add("going", ago(time.Hour), true, managed())
	f.terminate(f.snaps["going"])
	f.addFailed("failed-going", ago(time.Hour), managed())
	f.terminate(f.snaps["failed-going"])
	f.add("good-b", ago(2*time.Hour), true, managed())
	f.add("good-a", ago(3*time.Hour), true, managed())
	s := newSnapshotter(t, f)
	s.Keep = 2
	deleted, err := s.Prune(context.Background())
	if err != nil || len(deleted) != 0 {
		t.Fatalf("want nothing deleted, got %v %v", deleted, err)
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "DELETE") || strings.HasPrefix(call, "PATCH") {
			t.Fatalf("unexpected call %s", call)
		}
	}
}

// Pruning never removes the last usable snapshot, whatever Keep says.
func TestPruneKeepsAtLeastOne(t *testing.T) {
	f := newFakeAPI()
	f.add("only", ago(time.Hour), true, managed())
	s := newSnapshotter(t, f)
	s.Keep = 0
	if deleted, err := s.Prune(context.Background()); err != nil || len(deleted) != 0 {
		t.Fatalf("want nothing deleted, got %v %v", deleted, err)
	}
}

// A failed snapshot whose status never named its content still takes its storage
// with it: the content is found by the controller's naming and its back-reference.
func TestDeleteFindsUnboundContent(t *testing.T) {
	t.Run("matching back-reference", func(t *testing.T) {
		f := newFakeAPI()
		f.addUnbound("failed", ago(time.Hour), contentRef{"failed", "gofer", "uid-failed"})
		s := newSnapshotter(t, f)
		if deleted, err := s.pruneFailed(context.Background()); err != nil || !slices.Equal(deleted, []string{"failed"}) {
			t.Fatalf("want it deleted, got %v %v", deleted, err)
		}
		if f.contents["snapcontent-uid-failed"] != "Delete" {
			t.Fatal("the unbound content must be set to Delete")
		}
	})
	t.Run("someone else's content", func(t *testing.T) {
		f := newFakeAPI()
		f.addUnbound("failed", ago(time.Hour), contentRef{"failed", "gofer", "uid-of-an-older-object"})
		s := newSnapshotter(t, f)
		if deleted, err := s.pruneFailed(context.Background()); err != nil || !slices.Equal(deleted, []string{"failed"}) {
			t.Fatalf("want it deleted, got %v %v", deleted, err)
		}
		if f.contents["snapcontent-uid-failed"] != "Retain" {
			t.Fatal("a content that does not refer back to the snapshot must not be touched")
		}
	})
	t.Run("lookup fails", func(t *testing.T) {
		f := newFakeAPI()
		f.addUnbound("failed", ago(time.Hour), contentRef{"failed", "gofer", "uid-failed"})
		f.failGetOf = "snapcontent-uid-failed"
		s := newSnapshotter(t, f)
		if _, err := s.pruneFailed(context.Background()); err == nil {
			t.Fatal("want the lookup error")
		}
		if _, ok := f.snaps["failed"]; !ok {
			t.Fatal("unsure where its storage is, the snapshot must be kept")
		}
	})
	t.Run("no content", func(t *testing.T) {
		f := newFakeAPI()
		f.addUnbound("failed", ago(time.Hour), contentRef{})
		delete(f.refs, "snapcontent-uid-failed")
		s := newSnapshotter(t, f)
		if deleted, err := s.pruneFailed(context.Background()); err != nil || !slices.Equal(deleted, []string{"failed"}) {
			t.Fatalf("want it deleted, got %v %v", deleted, err)
		}
	})
}

// A run whose snapshot has been failing for the grace period when the wait ends
// deletes it (and its stored content) and any earlier failed one, but prunes nothing
// else, so a broken backup cannot delete good ones.
func TestRunDeletesItsFailedSnapshot(t *testing.T) {
	f := newFakeAPI()
	f.readyAt, f.failWith = -1, "boom"
	for i := range 4 {
		f.add("old-"+string(rune('a'+i)), ago(time.Duration(i+1)*time.Hour), true, managed())
	}
	f.addFailed("earlier-failed", ago(30*time.Minute), managed())
	s := newSnapshotter(t, f)
	s.Keep = 1
	later(s)
	err := s.Run(context.Background(), 50*time.Millisecond)
	var failing *FailedError
	if !errors.As(err, &failing) || failing.Message != "boom" {
		t.Fatalf("want the failing snapshot to fail the run, got %v", err)
	}
	if _, ok := f.snaps[newName]; ok {
		t.Fatal("the run's failed snapshot must be deleted")
	}
	if f.contents["content-"+newName] != "Delete" {
		t.Fatalf("the failed snapshot's content must be set to Delete, got %q", f.contents["content-"+newName])
	}
	if _, ok := f.snaps["earlier-failed"]; ok {
		t.Fatal("an earlier failed snapshot must be deleted too")
	}
	for i := range 4 {
		if _, ok := f.snaps["old-"+string(rune('a'+i))]; !ok {
			t.Fatalf("no usable snapshot may be pruned after a failed one, have %v", keys(f.snaps))
		}
	}
}

// A run's snapshot whose error has not yet lasted the grace period is left for the
// next run to judge, though the run still fails and earlier failed ones still go.
func TestRunLeavesANewlyFailingSnapshot(t *testing.T) {
	f := newFakeAPI()
	f.readyAt, f.failWith = -1, "boom"
	f.addFailed("earlier-failed", ago(time.Hour), managed())
	s := newSnapshotter(t, f)
	var failing *FailedError
	if err := s.Run(context.Background(), 20*time.Millisecond); !errors.As(err, &failing) {
		t.Fatalf("want the failing snapshot to fail the run, got %v", err)
	}
	if _, ok := f.snaps[newName]; !ok || f.contents["content-"+newName] != "Retain" {
		t.Fatal("a snapshot whose error is younger than the grace period must be kept")
	}
	if !slices.Equal(f.deletes(), []string{"earlier-failed"}) {
		t.Fatalf("want only the earlier failed snapshot deleted, deleted %v", f.deletes())
	}
}

// A snapshot that reports an error and then succeeds on the controller's retry is a
// good backup, and the run carries on as usual.
func TestRunKeepsASnapshotThatRecovers(t *testing.T) {
	f := newFakeAPI()
	f.readyAt, f.failWith = 1, "transient"
	f.add("old-a", ago(time.Hour), true, managed())
	f.add("old-b", ago(2*time.Hour), true, managed())
	s := newSnapshotter(t, f)
	s.Keep = 2
	if err := s.Run(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.snaps[newName]; !ok || f.contents["content-"+newName] != "Retain" {
		t.Fatal("the recovered snapshot must stay, with its storage retained")
	}
	if !slices.Equal(f.deletes(), []string{"old-b"}) {
		t.Fatalf("want only the oldest beyond Keep pruned, deleted %v", f.deletes())
	}
}

// A run whose snapshot is merely slow leaves it in place (it may still finish) and
// prunes nothing usable, but still clears out snapshots that have failed.
func TestRunLeavesASlowSnapshot(t *testing.T) {
	f := newFakeAPI()
	f.readyAt = -1
	for i := range 3 {
		f.add("old-"+string(rune('a'+i)), ago(time.Duration(i+1)*time.Hour), true, managed())
	}
	f.addFailed("earlier-failed", ago(30*time.Minute), managed())
	s := newSnapshotter(t, f)
	s.Keep = 1
	err := s.Run(context.Background(), 20*time.Millisecond)
	var failing *FailedError
	if err == nil || errors.As(err, &failing) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want a timeout, got %v", err)
	}
	if _, ok := f.snaps[newName]; !ok {
		t.Fatal("a snapshot that is only slow must be left to finish")
	}
	if f.contents["content-"+newName] != "Retain" {
		t.Fatal("a slow snapshot's content must stay Retain")
	}
	if _, ok := f.snaps["earlier-failed"]; ok {
		t.Fatal("an earlier failed snapshot must be deleted")
	}
	if len(f.snaps) != 4 {
		t.Fatalf("no usable snapshot may be pruned after a slow run, have %v", keys(f.snaps))
	}
}

// If someone starts deleting the run's snapshot while it waits, their deletion stands
// as they made it: the run does not switch its storage from Retain to Delete.
func TestRunLeavesAnOperatorsDeletionAlone(t *testing.T) {
	f := newFakeAPI()
	f.readyAt, f.failWith = -1, "boom"
	f.onGet = func(name string, obj map[string]any, n int) {
		if name == newName && n >= 2 {
			f.terminate(obj)
		}
	}
	s := newSnapshotter(t, f)
	later(s) // otherwise it would be failed, and deleted
	var failing *FailedError
	if err := s.Run(context.Background(), 30*time.Millisecond); !errors.As(err, &failing) {
		t.Fatalf("want the failing snapshot to fail the run, got %v", err)
	}
	if f.contents["content-"+newName] != "Retain" {
		t.Fatal("an operator's deletion must keep the storage policy they chose")
	}
	if len(f.deletes()) != 0 {
		t.Fatalf("the run must not delete a snapshot already being deleted, deleted %v", f.deletes())
	}
}

// If the snapshot cannot even be created (an earlier failed one may be what fills the
// namespace's snapshot quota), failed snapshots are still cleaned up.
func TestRunCleansUpWhenCreateIsRejected(t *testing.T) {
	f := newFakeAPI()
	f.reject = "exceeded quota: count/volumesnapshots.snapshot.storage.k8s.io"
	f.addFailed("earlier-failed", ago(time.Hour), managed())
	f.add("good", ago(2*time.Hour), true, managed())
	s := newSnapshotter(t, f)
	s.Keep = 1
	err := s.Run(context.Background(), time.Second)
	if err == nil || !strings.Contains(err.Error(), "exceeded quota") {
		t.Fatalf("want the create error, got %v", err)
	}
	if _, ok := f.snaps["earlier-failed"]; ok {
		t.Fatal("the failed snapshot must be deleted")
	}
	if _, ok := f.snaps["good"]; !ok {
		t.Fatal("a usable snapshot must stay")
	}
}

// When the job itself is stopped, the run does not go on deleting things.
func TestRunCancelledCleansNothing(t *testing.T) {
	f := newFakeAPI()
	f.readyAt, f.failWith = -1, "boom"
	f.addFailed("earlier-failed", ago(time.Hour), managed())
	s := newSnapshotter(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	if err := s.Run(ctx, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("want cancellation, got %v", err)
	}
	if len(f.deletes()) != 0 {
		t.Fatalf("a cancelled run must not clean up, deleted %v", f.deletes())
	}
}

func TestRunTakesWaitsAndPrunes(t *testing.T) {
	f := newFakeAPI()
	f.readyAt = 0
	for i := range 3 {
		f.add("old-"+string(rune('a'+i)), time.Date(2026, 11, 1+i, 0, 0, 0, 0, time.UTC), true, managed())
	}
	s := newSnapshotter(t, f)
	s.Keep = 2
	if err := s.Run(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	// The new one (ready) and the newest old one stay.
	if len(f.snaps) != 2 {
		t.Fatalf("want 2 snapshots left, have %v", keys(f.snaps))
	}
	if _, ok := f.snaps[newName]; !ok {
		t.Fatal("the new snapshot must stay")
	}
}

func keys(m map[string]map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestStatusErrors(t *testing.T) {
	f := newFakeAPI()
	s := newSnapshotter(t, f)
	f.token = "other"
	_, err := s.Take(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want a 401, got %v", err)
	}
	if !IsNotFound(&StatusError{Code: 404}) || IsNotFound(&StatusError{Code: 500}) {
		t.Fatal("IsNotFound")
	}
}

// labelOf reads a label from a stored object, whether it was seeded by the test
// (map[string]string) or decoded from a request (map[string]any).
func labelOf(obj map[string]any, key string) string {
	switch labels := obj["metadata"].(map[string]any)["labels"].(type) {
	case map[string]string:
		return labels[key]
	case map[string]any:
		v, _ := labels[key].(string)
		return v
	}
	return ""
}
