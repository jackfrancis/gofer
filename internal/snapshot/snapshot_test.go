package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPI is a tiny stand-in for the API server's snapshot endpoints. It applies the
// label selector the way the API server would, and records every call.
type fakeAPI struct {
	mu        sync.Mutex
	snaps     map[string]map[string]any // name -> object
	contents  map[string]string         // content name -> deletionPolicy
	calls     []string
	token     string
	readyAt   int // GETs of a snapshot before it reports ready; -1 never
	failWith  string
	getsByKey map[string]int
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{snaps: map[string]map[string]any{}, contents: map[string]string{}, token: "tok", getsByKey: map[string]int{}}
}

func (f *fakeAPI) add(name string, created time.Time, ready bool, labels map[string]string) {
	f.snaps[name] = map[string]any{
		"metadata": map[string]any{"name": name, "labels": labels, "creationTimestamp": created.Format(time.RFC3339)},
		"status":   map[string]any{"readyToUse": ready, "boundVolumeSnapshotContentName": "content-" + name, "restoreSize": "1Gi"},
	}
	f.contents["content-"+name] = "Retain"
}

// addFailed seeds a snapshot that reports an error and is not ready.
func (f *fakeAPI) addFailed(name string, created time.Time, labels map[string]string) {
	f.add(name, created, false, labels)
	f.snaps[name]["status"].(map[string]any)["error"] = map[string]any{"message": "boom"}
}

func managed() map[string]string {
	return map[string]string{LabelManagedBy: ManagedBy, LabelClaim: "data-gofer-0"}
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	const snapPrefix = "/apis/snapshot.storage.k8s.io/v1/namespaces/gofer/volumesnapshots"
	const contentPrefix = "/apis/snapshot.storage.k8s.io/v1/volumesnapshotcontents/"
	switch {
	case r.Method == "POST" && r.URL.Path == snapPrefix:
		var obj map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &obj)
		meta := obj["metadata"].(map[string]any)
		name := meta["name"].(string)
		meta["creationTimestamp"] = time.Date(2026, 11, 17, 19, 30, 5, 0, time.UTC).Format(time.RFC3339) // set by the API server
		f.snaps[name] = obj
		f.contents["content-"+name] = "Retain" // the class's deletionPolicy, as the controller would bind it
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
		status := map[string]any{
			"readyToUse":                     f.readyAt >= 0 && f.getsByKey[name] > f.readyAt,
			"restoreSize":                    "1Gi",
			"boundVolumeSnapshotContentName": "content-" + name,
		}
		if f.failWith != "" {
			status["error"] = map[string]any{"message": f.failWith}
		}
		obj["status"] = status
		_ = json.NewEncoder(w).Encode(obj)
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, snapPrefix+"/"):
		name := strings.TrimPrefix(r.URL.Path, snapPrefix+"/")
		delete(f.snaps, name)
		w.WriteHeader(http.StatusOK)
	case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, contentPrefix):
		if r.Header.Get("Content-Type") != "application/merge-patch+json" {
			http.Error(w, `{"message":"bad patch type"}`, http.StatusUnsupportedMediaType)
			return
		}
		var patch struct {
			Spec struct {
				DeletionPolicy string `json:"deletionPolicy"`
			} `json:"spec"`
		}
		_ = json.NewDecoder(r.Body).Decode(&patch)
		f.contents[strings.TrimPrefix(r.URL.Path, contentPrefix)] = patch.Spec.DeletionPolicy
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, `{"message":"unexpected"}`, http.StatusTeapot)
	}
}

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
		Now:       func() time.Time { return time.Date(2026, 11, 17, 19, 30, 5, 0, time.UTC) },
	}
}

func TestTakeCreatesALabeledSnapshot(t *testing.T) {
	f := newFakeAPI()
	s := newSnapshotter(t, f)
	name, err := s.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if name != "data-gofer-0-20261117-193005" {
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
	t.Run("ready", func(t *testing.T) {
		f := newFakeAPI()
		f.readyAt = 2
		s := newSnapshotter(t, f)
		name, _ := s.Take(context.Background())
		size, err := s.Wait(context.Background(), name)
		if err != nil || size != "1Gi" {
			t.Fatalf("want ready with a size, got %q %v", size, err)
		}
	})
	t.Run("failed", func(t *testing.T) {
		f := newFakeAPI()
		f.readyAt, f.failWith = -1, "quota exceeded"
		s := newSnapshotter(t, f)
		name, _ := s.Take(context.Background())
		if _, err := s.Wait(context.Background(), name); err == nil || !strings.Contains(err.Error(), "quota exceeded") {
			t.Fatalf("want the snapshot's error, got %v", err)
		}
	})
	t.Run("ready despite a stale error", func(t *testing.T) {
		f := newFakeAPI()
		f.readyAt, f.failWith = 0, "transient"
		s := newSnapshotter(t, f)
		name, _ := s.Take(context.Background())
		if size, err := s.Wait(context.Background(), name); err != nil || size != "1Gi" {
			t.Fatalf("a snapshot ready to use is usable, got %q %v", size, err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		f := newFakeAPI()
		f.readyAt = -1
		s := newSnapshotter(t, f)
		name, _ := s.Take(context.Background())
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		if _, err := s.Wait(ctx, name); err == nil {
			t.Fatal("want a timeout")
		}
	})
}

func TestPrune(t *testing.T) {
	f := newFakeAPI()
	base := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	day := func(n int) time.Time { return base.Add(time.Duration(n) * 24 * time.Hour) }
	f.add("in-progress", day(6), false, managed())      // newest, not ready yet: keep
	f.add("good-5", day(5), true, managed())            // usable #1: keep
	f.add("failed-4", day(4), false, managed())         // between keepers: keep
	f.add("good-3", day(3), true, managed())            // usable #2: keep
	f.add("good-2", day(2), true, managed())            // beyond Keep=2: delete
	f.add("failed-1", day(1), false, managed())         // beyond: delete
	f.add("by-hand", day(0), true, map[string]string{}) // not managed: never touched
	f.add("other-claim", day(0), true, map[string]string{LabelManagedBy: ManagedBy, LabelClaim: "other"})
	s := newSnapshotter(t, f)

	deleted, err := s.Prune(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(deleted)
	if !slices.Equal(deleted, []string{"failed-1", "good-2"}) {
		t.Fatalf("deleted %v", deleted)
	}
	for _, name := range []string{"in-progress", "good-5", "failed-4", "good-3", "by-hand", "other-claim"} {
		if _, ok := f.snaps[name]; !ok {
			t.Errorf("%s must be kept", name)
		}
	}
	// Each pruned snapshot's stored content is switched to Delete first, so the
	// storage goes with it; kept ones stay Retain.
	if f.contents["content-good-2"] != "Delete" || f.contents["content-failed-1"] != "Delete" {
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

// Failed snapshots go wherever they sit; in-progress ones inside the window and
// usable ones up to Keep stay.
func TestPruneDeletesFailedAnywhere(t *testing.T) {
	f := newFakeAPI()
	base := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	day := func(n int) time.Time { return base.Add(time.Duration(n) * 24 * time.Hour) }
	f.addFailed("failed-8", day(8), managed())     // newest, failed: delete
	f.add("in-progress", day(7), false, managed()) // not ready, no error: keep
	f.add("stale-error", day(6), true, managed())  // ready with an old error: usable #1, keep
	f.snaps["stale-error"]["status"].(map[string]any)["error"] = map[string]any{"message": "transient"}
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
	for _, name := range []string{"in-progress", "stale-error", "good-4", "by-hand"} {
		if _, ok := f.snaps[name]; !ok {
			t.Errorf("%s must be kept", name)
		}
	}
	if f.contents["content-failed-5"] != "Delete" {
		t.Fatal("a failed snapshot's stored content must go with it")
	}
}

// Deleting failed snapshots never lowers the number of usable ones below Keep.
func TestPruneFailedNeverCostsAUsableOne(t *testing.T) {
	f := newFakeAPI()
	base := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	for i := range 3 {
		f.addFailed("failed-"+string(rune('a'+i)), base.Add(time.Duration(2*i)*time.Hour), managed())
		f.add("good-"+string(rune('a'+i)), base.Add(time.Duration(2*i+1)*time.Hour), true, managed())
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

// pruneFailed touches only failed snapshots, never usable or in-progress ones.
func TestPruneFailedOnly(t *testing.T) {
	f := newFakeAPI()
	base := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	for i := range 4 {
		f.add("good-"+string(rune('a'+i)), base.Add(time.Duration(i)*time.Hour), true, managed())
	}
	f.add("old-in-progress", base.Add(-time.Hour), false, managed())
	f.addFailed("failed", base.Add(-2*time.Hour), managed())
	s := newSnapshotter(t, f)
	s.Keep = 1
	deleted, err := s.pruneFailed(context.Background())
	if err != nil || !slices.Equal(deleted, []string{"failed"}) {
		t.Fatalf("want only the failed one deleted, got %v %v", deleted, err)
	}
	if len(f.snaps) != 5 {
		t.Fatalf("have %v", keys(f.snaps))
	}
}

// A snapshot already being deleted is neither counted as a backup nor deleted again.
func TestPruneSkipsTerminating(t *testing.T) {
	f := newFakeAPI()
	base := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	f.add("going", base.Add(3*time.Hour), true, managed())
	f.snaps["going"]["metadata"].(map[string]any)["deletionTimestamp"] = base.Format(time.RFC3339)
	f.add("good-b", base.Add(2*time.Hour), true, managed())
	f.add("good-a", base.Add(time.Hour), true, managed())
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
	f.add("only", time.Now(), true, managed())
	s := newSnapshotter(t, f)
	s.Keep = 0
	if deleted, err := s.Prune(context.Background()); err != nil || len(deleted) != 0 {
		t.Fatalf("want nothing deleted, got %v %v", deleted, err)
	}
}

// A run whose snapshot fails deletes that snapshot (and its stored content) and any
// earlier failed one, but prunes nothing else, so a broken backup cannot delete good
// ones.
func TestRunDeletesItsFailedSnapshot(t *testing.T) {
	f := newFakeAPI()
	f.readyAt, f.failWith = -1, "boom"
	for i := range 4 {
		f.add("old-"+string(rune('a'+i)), time.Now().Add(-time.Duration(i+1)*time.Hour), true, managed())
	}
	f.addFailed("earlier-failed", time.Now().Add(-30*time.Minute), managed())
	s := newSnapshotter(t, f)
	s.Keep = 1
	err := s.Run(context.Background(), time.Second)
	var failed *FailedError
	if !errors.As(err, &failed) || failed.Message != "boom" {
		t.Fatalf("want the failed snapshot to fail the run, got %v", err)
	}
	const name = "data-gofer-0-20261117-193005"
	if _, ok := f.snaps[name]; ok {
		t.Fatal("the run's failed snapshot must be deleted")
	}
	if f.contents["content-"+name] != "Delete" {
		t.Fatalf("the failed snapshot's content must be set to Delete, got %q", f.contents["content-"+name])
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

// A run whose snapshot is merely slow leaves it in place (it may still finish) and
// prunes nothing usable, but still clears out snapshots that have failed.
func TestRunLeavesASlowSnapshot(t *testing.T) {
	f := newFakeAPI()
	f.readyAt = -1
	for i := range 3 {
		f.add("old-"+string(rune('a'+i)), time.Now().Add(-time.Duration(i+1)*time.Hour), true, managed())
	}
	f.addFailed("earlier-failed", time.Now().Add(-30*time.Minute), managed())
	s := newSnapshotter(t, f)
	s.Keep = 1
	err := s.Run(context.Background(), 20*time.Millisecond)
	var failed *FailedError
	if err == nil || errors.As(err, &failed) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want a timeout, got %v", err)
	}
	const name = "data-gofer-0-20261117-193005"
	if _, ok := f.snaps[name]; !ok {
		t.Fatal("a snapshot that is only slow must be left to finish")
	}
	if f.contents["content-"+name] != "Retain" {
		t.Fatal("a slow snapshot's content must stay Retain")
	}
	if _, ok := f.snaps["earlier-failed"]; ok {
		t.Fatal("an earlier failed snapshot must be deleted")
	}
	if len(f.snaps) != 4 {
		t.Fatalf("no usable snapshot may be pruned after a slow run, have %v", keys(f.snaps))
	}
}

// When the job itself is stopped, the run does not go on deleting things.
func TestRunCancelledCleansNothing(t *testing.T) {
	f := newFakeAPI()
	f.readyAt = -1
	f.addFailed("earlier-failed", time.Now().Add(-30*time.Minute), managed())
	s := newSnapshotter(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	if err := s.Run(ctx, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("want cancellation, got %v", err)
	}
	if _, ok := f.snaps["earlier-failed"]; !ok {
		t.Fatal("a cancelled run must not clean up")
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
	if _, ok := f.snaps["data-gofer-0-20261117-193005"]; !ok {
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
