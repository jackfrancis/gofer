// Package snapshot takes and prunes CSI VolumeSnapshots of gofer's data volume.
//
// gofer keeps its state in one SQLite database on one volume (internal/storage). A
// CSI snapshot of that volume is a crash-consistent image of the disk, which SQLite in
// WAL mode with synchronous=FULL restores to the last committed transaction, so a
// snapshot of the live volume is a valid backup with no quiescing
// (docs/backup-restore.md). This package is what the scheduled backup runs: take one
// snapshot, wait until it is usable, and delete the oldest beyond a retention count.
//
// It is cloud-neutral. Which storage the snapshot lands in, and where, is the
// VolumeSnapshotClass's business, which the cluster's administrator supplies.
package snapshot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"time"
)

// Labels on every snapshot this package takes. Pruning only ever considers snapshots
// carrying both, so snapshots made by hand are never deleted by it.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	ManagedBy      = "gofer-snapshot"
	LabelClaim     = "gofer.dev/claim"
)

const snapshotAPI = "/apis/snapshot.storage.k8s.io/v1"

// Snapshotter takes and prunes snapshots of one claim.
type Snapshotter struct {
	Kube      *Kube
	Namespace string
	Claim     string // the PersistentVolumeClaim to snapshot, e.g. data-gofer-0
	Class     string // the VolumeSnapshotClass to use
	Keep      int    // how many usable snapshots to keep
	Poll      time.Duration
	Log       *slog.Logger
	Now       func() time.Time
}

// volumeSnapshot is the part of a VolumeSnapshot this package reads and writes.
type volumeSnapshot struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name              string            `json:"name"`
		Namespace         string            `json:"namespace,omitempty"`
		Labels            map[string]string `json:"labels,omitempty"`
		CreationTimestamp time.Time         `json:"creationTimestamp,omitzero"`
	} `json:"metadata"`
	Spec struct {
		VolumeSnapshotClassName string `json:"volumeSnapshotClassName,omitempty"`
		Source                  struct {
			PersistentVolumeClaimName string `json:"persistentVolumeClaimName,omitempty"`
		} `json:"source"`
	} `json:"spec"`
	Status *struct {
		ReadyToUse                     *bool  `json:"readyToUse,omitempty"`
		BoundVolumeSnapshotContentName string `json:"boundVolumeSnapshotContentName,omitempty"`
		RestoreSize                    string `json:"restoreSize,omitempty"`
		Error                          *struct {
			Message string `json:"message,omitempty"`
		} `json:"error,omitempty"`
	} `json:"status,omitempty"`
}

func (v volumeSnapshot) ready() bool {
	return v.Status != nil && v.Status.ReadyToUse != nil && *v.Status.ReadyToUse
}

func (v volumeSnapshot) failure() string {
	if v.Status != nil && v.Status.Error != nil && v.Status.Error.Message != "" {
		return v.Status.Error.Message
	}
	return ""
}

func (s *Snapshotter) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Snapshotter) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Snapshotter) path(name string) string {
	p := snapshotAPI + "/namespaces/" + url.PathEscape(s.Namespace) + "/volumesnapshots"
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p
}

// Name is the snapshot name for a claim at a time: the claim name and the UTC time,
// so names sort by age and say what they are.
func Name(claim string, t time.Time) string {
	return claim + "-" + t.UTC().Format("20060102-150405")
}

// Take creates a snapshot of the claim and returns its name.
func (s *Snapshotter) Take(ctx context.Context) (string, error) {
	if s.Class == "" {
		return "", errors.New("snapshot: no VolumeSnapshotClass configured")
	}
	var v volumeSnapshot
	v.APIVersion = "snapshot.storage.k8s.io/v1"
	v.Kind = "VolumeSnapshot"
	v.Metadata.Name = Name(s.Claim, s.now())
	v.Metadata.Namespace = s.Namespace
	v.Metadata.Labels = map[string]string{LabelManagedBy: ManagedBy, LabelClaim: s.Claim}
	v.Spec.VolumeSnapshotClassName = s.Class
	v.Spec.Source.PersistentVolumeClaimName = s.Claim
	if err := s.Kube.do(ctx, "POST", s.path(""), "application/json", v, nil); err != nil {
		return "", fmt.Errorf("snapshot: create %s: %w", v.Metadata.Name, err)
	}
	s.log().Info("snapshot requested", "snapshot", v.Metadata.Name, "claim", s.Claim, "class", s.Class)
	return v.Metadata.Name, nil
}

// Wait polls the snapshot until it is ready to use, reports an error, or ctx ends.
// It returns the snapshot's restore size when ready.
func (s *Snapshotter) Wait(ctx context.Context, name string) (string, error) {
	poll := s.Poll
	if poll <= 0 {
		poll = 5 * time.Second
	}
	for {
		var v volumeSnapshot
		if err := s.Kube.do(ctx, "GET", s.path(name), "", nil, &v); err != nil {
			return "", fmt.Errorf("snapshot: read %s: %w", name, err)
		}
		if msg := v.failure(); msg != "" {
			return "", fmt.Errorf("snapshot: %s failed: %s", name, msg)
		}
		if v.ready() {
			return v.Status.RestoreSize, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("snapshot: %s not ready: %w", name, ctx.Err())
		case <-time.After(poll):
		}
	}
}

// Prune deletes this claim's managed snapshots beyond the newest Keep usable ones.
// Snapshots newer than the Keep-th usable one are left alone even if not yet usable
// (they may still be in progress); everything older goes, usable or failed. It
// returns the names it deleted. Keep below 1 is treated as 1: pruning never removes
// the last good backup.
func (s *Snapshotter) Prune(ctx context.Context) ([]string, error) {
	keep := max(s.Keep, 1)
	sel := url.Values{"labelSelector": {LabelManagedBy + "=" + ManagedBy + "," + LabelClaim + "=" + s.Claim}}
	var list struct {
		Items []volumeSnapshot `json:"items"`
	}
	if err := s.Kube.do(ctx, "GET", s.path("")+"?"+sel.Encode(), "", nil, &list); err != nil {
		return nil, fmt.Errorf("snapshot: list: %w", err)
	}
	items := list.Items
	sort.SliceStable(items, func(i, j int) bool { // newest first
		a, b := items[i].Metadata, items[j].Metadata
		if !a.CreationTimestamp.Equal(b.CreationTimestamp) {
			return a.CreationTimestamp.After(b.CreationTimestamp)
		}
		return a.Name > b.Name
	})
	var (
		usable  int
		deleted []string
		errs    []error
	)
	for _, v := range items {
		if usable < keep {
			if v.ready() {
				usable++
			}
			continue
		}
		if err := s.delete(ctx, v); err != nil {
			errs = append(errs, err)
			continue
		}
		deleted = append(deleted, v.Metadata.Name)
	}
	if len(deleted) > 0 {
		s.log().Info("snapshots pruned", "deleted", deleted, "kept_usable", usable)
	}
	return deleted, errors.Join(errs...)
}

// delete removes one snapshot AND the storage behind it. The snapshot class is
// expected to use deletionPolicy Retain, so that deleting the gofer namespace (or a
// snapshot object by accident) keeps the stored snapshot. Pruning is a deliberate
// deletion, so it first switches the bound VolumeSnapshotContent to Delete; deleting
// the VolumeSnapshot then removes the content and the stored snapshot with it.
func (s *Snapshotter) delete(ctx context.Context, v volumeSnapshot) error {
	if v.Status != nil && v.Status.BoundVolumeSnapshotContentName != "" {
		content := v.Status.BoundVolumeSnapshotContentName
		patch := map[string]any{"spec": map[string]any{"deletionPolicy": "Delete"}}
		err := s.Kube.do(ctx, "PATCH", snapshotAPI+"/volumesnapshotcontents/"+url.PathEscape(content), "application/merge-patch+json", patch, nil)
		if err != nil && !IsNotFound(err) {
			return fmt.Errorf("snapshot: set %s to delete its storage: %w", content, err)
		}
	}
	if err := s.Kube.do(ctx, "DELETE", s.path(v.Metadata.Name), "", nil, nil); err != nil && !IsNotFound(err) {
		return fmt.Errorf("snapshot: delete %s: %w", v.Metadata.Name, err)
	}
	return nil
}

// Run is one scheduled backup: take a snapshot, wait up to wait for it to be usable,
// then prune. A snapshot that fails or does not become usable in time fails the run
// and skips pruning, so a broken backup never deletes good ones.
func (s *Snapshotter) Run(ctx context.Context, wait time.Duration) error {
	name, err := s.Take(ctx)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	size, err := s.Wait(wctx, name)
	if err != nil {
		return err
	}
	s.log().Info("snapshot ready", "snapshot", name, "restore_size", size)
	_, err = s.Prune(ctx)
	return err
}
