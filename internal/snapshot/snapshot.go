// Package snapshot takes and prunes CSI VolumeSnapshots of gofer's data volume.
//
// gofer keeps its state in one SQLite database on one volume (internal/storage). A
// CSI snapshot of that volume is a crash-consistent image of the disk, which SQLite in
// WAL mode with synchronous=FULL restores to the last committed transaction, so a
// snapshot of the live volume is a valid backup with no quiescing
// (docs/backup-restore.md). This package is what the scheduled backup runs: take one
// snapshot, wait until it is usable, and delete the oldest beyond a retention count,
// plus any that failed.
//
// Failed snapshots are deleted promptly, not left to age out: the external-snapshotter
// treats a snapshot that is not ready as still reading its source claim and keeps the
// claim's pvc-as-source-protection finalizer, so a failed snapshot left in place would
// block a restore (hack/restore/restore.sh refuses to run while one exists).
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
		DeletionTimestamp *time.Time        `json:"deletionTimestamp,omitempty"`
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

// failure is the snapshot's error if it has failed, else "". A snapshot that is ready
// to use has not failed, whatever stale error it still carries: readyToUse is what
// makes a backup usable here, in Prune, and in the restore preflight alike.
func (v volumeSnapshot) failure() string {
	if v.ready() || v.Status == nil || v.Status.Error == nil {
		return ""
	}
	if v.Status.Error.Message == "" {
		return "error reported without a message"
	}
	return v.Status.Error.Message
}

func (v volumeSnapshot) failed() bool { return v.failure() != "" }

// terminating reports whether the snapshot is already being deleted.
func (v volumeSnapshot) terminating() bool { return v.Metadata.DeletionTimestamp != nil }

// FailedError is Wait's answer for a snapshot that reports an error: it failed, as
// opposed to merely not being ready yet.
type FailedError struct {
	Name    string
	Message string
	snap    volumeSnapshot
}

func (e *FailedError) Error() string {
	return fmt.Sprintf("snapshot: %s failed: %s", e.Name, e.Message)
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
// It returns the snapshot's restore size when ready, and a *FailedError when the
// snapshot reports an error.
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
			return "", &FailedError{Name: name, Message: msg, snap: v}
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

// Prune deletes this claim's managed snapshots beyond the newest Keep usable ones,
// and every one that failed. It returns the names it deleted.
//
//   - A failed snapshot (one reporting an error and not ready to use) goes wherever it
//     sits: it is not a backup, and while it exists it keeps the claim protected as a
//     snapshot source, which blocks a restore.
//   - Snapshots newer than the Keep-th usable one are otherwise left alone even if not
//     yet usable: they may still be in progress.
//   - Everything older than the Keep-th usable one goes, usable or not.
//
// Deleting a failed snapshot never lowers the count of usable ones, so Keep usable
// snapshots survive whatever else is deleted. Keep below 1 is treated as 1: pruning
// never removes the last good backup. Snapshots already being deleted are skipped and
// not counted.
func (s *Snapshotter) Prune(ctx context.Context) ([]string, error) {
	return s.prune(ctx, false)
}

// pruneFailed deletes only this claim's managed snapshots that failed. A run whose
// own snapshot did not become usable does this instead of Prune: removing failed
// snapshots cannot touch a usable one, so it is safe after a broken backup.
func (s *Snapshotter) pruneFailed(ctx context.Context) ([]string, error) {
	return s.prune(ctx, true)
}

func (s *Snapshotter) prune(ctx context.Context, failedOnly bool) ([]string, error) {
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
		switch {
		case v.terminating():
			continue // already going; neither a backup to count nor one to delete
		case v.failed():
			// Not a backup, and it blocks a restore: delete it wherever it sits.
		case failedOnly:
			continue
		case usable < keep:
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
	switch {
	case len(deleted) == 0:
	case failedOnly:
		s.log().Info("failed snapshots deleted", "deleted", deleted)
	default:
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
// then prune. A snapshot that fails or does not become usable in time fails the run.
// Such a run does not Prune, so a broken backup never deletes good ones; it only
// deletes failed snapshots, which are never usable:
//
//   - If the new snapshot reports an error, it is deleted, with its stored content, so
//     it does not linger and block a restore.
//   - If it is merely not ready in time, it is left in place. A slow snapshot (a first
//     full copy of a large volume, a busy storage backend) may still complete and be a
//     good backup, and deleting it would throw that away. If it fails later, the next
//     run deletes it as failed; if it completes, it is a backup like any other.
//   - Either way, any earlier managed snapshot that has since failed is deleted too.
//
// If ctx itself ends (the job is being stopped), nothing is cleaned up.
func (s *Snapshotter) Run(ctx context.Context, wait time.Duration) error {
	name, err := s.Take(ctx)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	size, err := s.Wait(wctx, name)
	if err != nil {
		return errors.Join(err, s.afterFailure(ctx, err))
	}
	s.log().Info("snapshot ready", "snapshot", name, "restore_size", size)
	_, err = s.Prune(ctx)
	return err
}

// afterFailure cleans up after a run whose snapshot did not become usable, given
// Wait's error: it deletes the run's snapshot if it failed, then any other failed one.
func (s *Snapshotter) afterFailure(ctx context.Context, waitErr error) error {
	if ctx.Err() != nil {
		return nil
	}
	var errs []error
	var failed *FailedError
	if errors.As(waitErr, &failed) {
		if err := s.delete(ctx, failed.snap); err != nil {
			errs = append(errs, err)
		} else {
			s.log().Info("failed snapshot deleted", "snapshot", failed.Name, "error", failed.Message)
		}
	} else {
		s.log().Warn("snapshot not ready in time; leaving it to finish", "err", waitErr)
	}
	if _, err := s.pruneFailed(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
