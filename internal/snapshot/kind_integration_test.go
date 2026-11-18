//go:build kindintegration

package snapshot

// An opt-in check against a real cluster with the external-snapshotter and a CSI
// driver (the kind + csi-driver-host-path setup). It runs as the gofer-snapshotter
// ServiceAccount, so it exercises deploy/k8s/backup/rbac.yaml too. Environment:
//
//	GOFER_IT_SERVER  API server URL
//	GOFER_IT_CA      file with the cluster CA (PEM)
//	GOFER_IT_TOKEN   a gofer-snapshotter token
//	GOFER_IT_CLASS   a Retain VolumeSnapshotClass of a working driver
//	GOFER_IT_BOGUS   a VolumeSnapshotClass naming a driver nobody serves
//
// The namespace gofer must hold a bound claim snapcheck-0. The check snapshots and
// prunes only that claim, never gofer's own data-gofer-0, so the backups of a real
// gofer in the same namespace are never touched (pruning selects by the claim label).
// Still, run it on a throwaway cluster; one way to get there:
//
//	kind create cluster --name snapcheck --kubeconfig ./snapcheck.kubeconfig
//	export KUBECONFIG=./snapcheck.kubeconfig
//	# from a checkout of kubernetes-csi/external-snapshotter:
//	kubectl kustomize client/config/crd | kubectl create -f -
//	kubectl -n kube-system kustomize deploy/kubernetes/snapshot-controller | kubectl create -f -
//	# from a checkout of kubernetes-csi/csi-driver-host-path:
//	deploy/kubernetes-latest/deploy.sh
//	# from this repository:
//	kubectl apply -f internal/snapshot/testdata/kind-setup.yaml -f deploy/k8s/backup/rbac.yaml
//	kubectl config view --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d > ca.pem
//	GOFER_IT_SERVER=$(kubectl config view --raw -o jsonpath='{.clusters[0].cluster.server}') \
//	GOFER_IT_CA=ca.pem GOFER_IT_TOKEN=$(kubectl -n gofer create token gofer-snapshotter) \
//	GOFER_IT_CLASS=gofer-retain GOFER_IT_BOGUS=gofer-bogus \
//	go test -tags kindintegration -run TestKind -v ./internal/snapshot

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"
)

func itKube(t *testing.T) *Kube {
	t.Helper()
	ca, err := os.ReadFile(os.Getenv("GOFER_IT_CA"))
	if err != nil {
		t.Skip("GOFER_IT_CA not set")
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	tok := os.Getenv("GOFER_IT_TOKEN")
	return NewKube(os.Getenv("GOFER_IT_SERVER"), func() (string, error) { return tok, nil }, client)
}

// itClaim is the check's own claim: never gofer's data-gofer-0, whose backups a prune
// with Keep=1 would otherwise cut down to one.
const itClaim = "snapcheck-0"

func itSnapshotter(t *testing.T, class string) *Snapshotter {
	return &Snapshotter{Kube: itKube(t), Namespace: "gofer", Claim: itClaim, Class: class, Keep: 1, Poll: time.Second}
}

func raw(t *testing.T, k *Kube, path string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := k.do(context.Background(), "GET", path, "", nil, &m); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return m
}

func getSnap(t *testing.T, s *Snapshotter, name string) volumeSnapshot {
	t.Helper()
	var v volumeSnapshot
	if err := s.Kube.do(context.Background(), "GET", s.path(name), "", nil, &v); err != nil {
		t.Fatalf("GET %s: %v", name, err)
	}
	return v
}

// waitGone polls path until it is 404, and reports how long that took.
func waitGone(t *testing.T, k *Kube, path string, limit time.Duration) (time.Duration, bool) {
	start := time.Now()
	for time.Since(start) < limit {
		err := k.do(context.Background(), "GET", path, "", nil, nil)
		if IsNotFound(err) {
			return time.Since(start), true
		}
		time.Sleep(time.Second)
	}
	return limit, false
}

// managedSnaps lists the claim's managed snapshots that are not being deleted.
func managedSnaps(t *testing.T, s *Snapshotter) []volumeSnapshot {
	t.Helper()
	var list struct {
		Items []volumeSnapshot `json:"items"`
	}
	sel := url.Values{"labelSelector": {LabelManagedBy + "=" + ManagedBy + "," + LabelClaim + "=" + s.Claim}}
	if err := s.Kube.do(context.Background(), "GET", s.path("")+"?"+sel.Encode(), "", nil, &list); err != nil {
		t.Fatal(err)
	}
	var out []volumeSnapshot
	for _, v := range list.Items {
		if !v.terminating() {
			out = append(out, v)
		}
	}
	return out
}

// wantOnlyNewest checks that after a good run with Keep=1 only the newest snapshot is
// left: everything older was pruned (the prune must not lose a race with the
// controller).
func wantOnlyNewest(t *testing.T, s *Snapshotter) {
	t.Helper()
	left := managedSnaps(t, s)
	if len(left) != 1 {
		var names []string
		for _, v := range left {
			names = append(names, v.Metadata.Name)
		}
		t.Errorf("want only the newest snapshot left after pruning, have %v", names)
	}
}

func TestKind(t *testing.T) {
	ctx := context.Background()
	s := itSnapshotter(t, os.Getenv("GOFER_IT_CLASS"))

	// 1. A good snapshot: what the real status looks like once ready.
	time.Sleep(1100 * time.Millisecond) // names have one-second resolution
	good1, err := s.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	size, err := s.Wait(wctx, good1)
	cancel()
	if err != nil {
		t.Fatalf("first snapshot: %v", err)
	}
	v := getSnap(t, s, good1)
	b, _ := json.Marshal(v.Status)
	t.Logf("ready snapshot %s size %s status %s", good1, size, b)
	if !v.taken() {
		t.Error("a ready snapshot should have status.creationTime")
	}
	c, err := s.contentOf(ctx, v)
	if err != nil || c == nil {
		t.Fatalf("contentOf: %v %v", c, err)
	}
	t.Logf("content %s uid %s rv %s policy %s", c.name, c.uid, c.resourceVersion, c.policy)
	if c.policy != "Retain" {
		t.Errorf("want Retain from the class, got %s", c.policy)
	}

	// 2. Conditional content patches against the real API server.
	t.Run("patch with a stale resourceVersion", func(t *testing.T) {
		_, err := s.setPolicy(ctx, c.name, c.uid, c.resourceVersion+"0", "Delete")
		t.Logf("stale rv: %v", err)
		if !IsConflict(err) {
			t.Errorf("want 409, got %v", err)
		}
	})
	t.Run("patch with a wrong uid", func(t *testing.T) {
		_, err := s.setPolicy(ctx, c.name, "00000000-0000-0000-0000-000000000000", c.resourceVersion, "Delete")
		t.Logf("wrong uid: %v", err)
		if statusCode(err) != http.StatusUnprocessableEntity {
			t.Errorf("want 422 (the UID is immutable), got %v", err)
		}
	})
	t.Run("patch as checked, and back", func(t *testing.T) {
		rv, err := s.setPolicy(ctx, c.name, c.uid, c.resourceVersion, "Delete")
		if err != nil || rv == "" || rv == c.resourceVersion {
			t.Fatalf("want a new resourceVersion, got %q %v", rv, err)
		}
		if _, err := s.setPolicy(ctx, c.name, c.uid, rv, "Retain"); err != nil {
			t.Fatalf("back to Retain: %v", err)
		}
		if p := raw(t, s.Kube, snapshotAPI+"/volumesnapshotcontents/"+c.name)["spec"].(map[string]any)["deletionPolicy"]; p != "Retain" {
			t.Fatalf("policy now %v", p)
		}
	})
	t.Run("delete with a stale resourceVersion", func(t *testing.T) {
		opts := map[string]any{"kind": "DeleteOptions", "apiVersion": "v1",
			"preconditions": map[string]any{"uid": v.Metadata.UID, "resourceVersion": v.Metadata.ResourceVersion + "0"}}
		err := s.Kube.do(ctx, "DELETE", s.path(good1), "application/json", opts, nil)
		t.Logf("stale delete: %v", err)
		if !IsConflict(err) {
			t.Errorf("want 409, got %v", err)
		}
		if _, ok := waitGone(t, s.Kube, s.path(good1), 2*time.Second); ok {
			t.Fatal("the snapshot must still be there")
		}
	})

	// 3. A second good run prunes the first (Keep=1), storage included.
	time.Sleep(1100 * time.Millisecond) // names have one-second resolution
	if err := s.Run(ctx, 60*time.Second); err != nil {
		t.Fatalf("second run: %v", err)
	}
	wantOnlyNewest(t, s)
	took, ok := waitGone(t, s.Kube, s.path(good1), 60*time.Second)
	t.Logf("pruned snapshot gone after %s (%v)", took, ok)
	if !ok {
		t.Error("the pruned snapshot did not go")
	}
	took, ok = waitGone(t, s.Kube, snapshotAPI+"/volumesnapshotcontents/"+c.name, 60*time.Second)
	t.Logf("its content (storage) gone after %s (%v)", took, ok)
	if !ok {
		t.Error("the pruned snapshot's content did not go: storage left behind")
	}

	// 4. A snapshot that fails: of a claim that does not exist. What does the real
	// error look like, and does a run clean it up?
	t.Run("failed snapshot", func(t *testing.T) {
		f := itSnapshotter(t, os.Getenv("GOFER_IT_CLASS"))
		f.Claim = "no-such-claim"
		f.Grace = 3 * time.Second
		err := f.Run(ctx, 15*time.Second)
		var failing *FailedError
		t.Logf("run: %v", err)
		if !errors.As(err, &failing) {
			t.Fatalf("want a FailedError, got %v", err)
		}
		if _, ok := waitGone(t, f.Kube, f.path(failing.Name), 30*time.Second); !ok {
			v := getSnap(t, f, failing.Name)
			b, _ := json.Marshal(v)
			t.Fatalf("the failed snapshot was not deleted: %s", b)
		}
	})

	// 5. A snapshot that never finishes (no driver serves its class): left by the
	// run, then pruned once a newer usable one exists.
	t.Run("slow snapshot", func(t *testing.T) {
		b := itSnapshotter(t, os.Getenv("GOFER_IT_BOGUS"))
		time.Sleep(1100 * time.Millisecond)
		err := b.Run(ctx, 10*time.Second)
		var failing *FailedError
		if err == nil || errors.As(err, &failing) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("want a plain timeout, got %v", err)
		}
		var list struct {
			Items []volumeSnapshot `json:"items"`
		}
		sel := url.Values{"labelSelector": {LabelManagedBy + "=" + ManagedBy + "," + LabelClaim + "=" + itClaim}}
		if err := b.Kube.do(ctx, "GET", b.path("")+"?"+sel.Encode(), "", nil, &list); err != nil {
			t.Fatal(err)
		}
		var slow volumeSnapshot
		for _, it := range list.Items {
			if !it.ready() {
				slow = it
			}
		}
		if slow.Metadata.Name == "" {
			t.Fatal("the slow snapshot must be left in place")
		}
		bs, _ := json.Marshal(slow)
		t.Logf("slow snapshot: %s", bs)

		// A newer usable one, then prune with Keep=1.
		time.Sleep(1100 * time.Millisecond)
		if err := s.Run(ctx, 60*time.Second); err != nil {
			t.Fatalf("good run: %v", err)
		}
		wantOnlyNewest(t, s)
		took, ok := waitGone(t, s.Kube, s.path(slow.Metadata.Name), 60*time.Second)
		t.Logf("slow snapshot gone after %s (%v)", took, ok)
		if !ok {
			v := getSnap(t, s, slow.Metadata.Name)
			bs, _ := json.Marshal(v)
			t.Errorf("slow snapshot still there: %s", bs)
		}
	})
}
