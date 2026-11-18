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
// block a restore (hack/restore/restore.sh refuses to run while one exists). But an
// error is not final (the controller retries), so a snapshot counts as failed only
// once it has kept reporting one past a grace period.
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

// DefaultGrace is how long a snapshot may keep reporting an error before it counts as
// failed, when Snapshotter.Grace is unset.
const DefaultGrace = 10 * time.Minute

// Snapshotter takes and prunes snapshots of one claim.
type Snapshotter struct {
	Kube      *Kube
	Namespace string
	Claim     string        // the PersistentVolumeClaim to snapshot, e.g. data-gofer-0
	Class     string        // the VolumeSnapshotClass to use
	Keep      int           // how many usable snapshots to keep
	Grace     time.Duration // how long an erroring snapshot gets to recover (DefaultGrace if 0)
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
		UID               string            `json:"uid,omitempty"`
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

// erroring is the snapshot's latest creation error, or "" if it has none or is ready
// to use. An error is not final: the snapshot controller records its latest attempt in
// status.error, keeps retrying, and clears the error when an attempt succeeds. A
// snapshot that is ready to use is usable, whatever stale error it still carries:
// readyToUse is what makes a backup usable here, in Prune, and in the restore
// preflight alike.
func (v volumeSnapshot) erroring() string {
	if v.ready() || v.Status == nil || v.Status.Error == nil {
		return ""
	}
	if v.Status.Error.Message == "" {
		return "error reported without a message"
	}
	return v.Status.Error.Message
}

// terminating reports whether the snapshot is already being deleted.
func (v volumeSnapshot) terminating() bool { return v.Metadata.DeletionTimestamp != nil }

// failed reports whether a snapshot has given up: it reports an error and is older
// than the grace period, so the controller's retries have had that long to succeed.
func (s *Snapshotter) failed(v volumeSnapshot) bool {
	return v.erroring() != "" && s.now().Sub(v.Metadata.CreationTimestamp) >= s.grace()
}

func (s *Snapshotter) grace() time.Duration {
	if s.Grace > 0 {
		return s.Grace
	}
	return DefaultGrace
}

// FailedError is Wait's answer when the snapshot was still reporting an error when
// the wait ran out: it is failing, as opposed to merely not ready yet. It wraps the
// context's error.
type FailedError struct {
	Name    string
	Message string
	err     error
}

func (e *FailedError) Error() string {
	return fmt.Sprintf("snapshot: %s still failing when the wait ended: %s", e.Name, e.Message)
}

func (e *FailedError) Unwrap() error { return e.err }

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

// Wait polls the snapshot until it is ready to use or ctx ends, and returns its
// restore size when ready. An error the snapshot reports does not end the wait, since
// the controller retries; if the snapshot is still reporting one when ctx ends, Wait
// returns a *FailedError, otherwise an error wrapping ctx's.
func (s *Snapshotter) Wait(ctx context.Context, name string) (string, error) {
	poll := s.Poll
	if poll <= 0 {
		poll = 5 * time.Second
	}
	var lastErr string // the error the latest successful read reported
	for {
		var v volumeSnapshot
		err := s.Kube.do(ctx, "GET", s.path(name), "", nil, &v)
		switch {
		case err != nil && ctx.Err() == nil:
			return "", fmt.Errorf("snapshot: read %s: %w", name, err)
		case err != nil:
			// The read was cut short by ctx ending; judge by the last one that landed.
		case v.ready():
			return v.Status.RestoreSize, nil
		default:
			msg := v.erroring()
			if msg != "" && msg != lastErr {
				s.log().Warn("snapshot reports an error; waiting for the controller to retry", "snapshot", name, "error", msg)
			}
			lastErr = msg
		}
		select {
		case <-ctx.Done():
			if lastErr != "" {
				return "", &FailedError{Name: name, Message: lastErr, err: ctx.Err()}
			}
			return "", fmt.Errorf("snapshot: %s not ready: %w", name, ctx.Err())
		case <-time.After(poll):
		}
	}
}

// Prune deletes this claim's managed snapshots beyond the newest Keep usable ones,
// and every one that failed. It returns the names it deleted.
//
//   - A failed snapshot (one still reporting an error after the grace period, see
//     Snapshotter.Grace) goes wherever it sits: it is not a backup, and while it
//     exists it keeps the claim protected as a snapshot source, which blocks a restore.
//   - Snapshots newer than the Keep-th usable one are otherwise left alone even if not
//     yet usable: they may still be in progress, or retrying an error.
//   - Everything older than the Keep-th usable one goes, usable or not.
//
// Deleting a failed snapshot never lowers the count of usable ones, so Keep usable
// snapshots survive whatever else is deleted. Keep below 1 is treated as 1: pruning
// never removes the last good backup. Snapshots already being deleted are skipped and
// not counted.
func (s *Snapshotter) Prune(ctx context.Context) ([]string, error) {
	return s.prune(ctx, false)
}

// pruneFailed deletes only this claim's managed snapshots that failed. A run that
// produced no usable snapshot does this instead of Prune: removing failed snapshots
// cannot touch a usable one, so it is safe after a broken backup.
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
		var still func(volumeSnapshot) bool // re-checked on a fresh read just before deleting
		switch {
		case v.terminating():
			continue // already going; neither a backup to count nor one to delete
		case s.failed(v):
			// Not a backup, and it blocks a restore: delete it wherever it sits, as long
			// as it has not recovered in the meantime.
			still = s.failed
		case failedOnly:
			continue
		case usable < keep:
			if v.ready() {
				usable++
			}
			continue
		}
		gone, err := s.delete(ctx, v.Metadata.Name, still)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if gone {
			deleted = append(deleted, v.Metadata.Name)
		}
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

// volumeSnapshotContent is the part of a VolumeSnapshotContent this package reads.
type volumeSnapshotContent struct {
	Spec struct {
		VolumeSnapshotRef struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
			UID       string `json:"uid"`
		} `json:"volumeSnapshotRef"`
	} `json:"spec"`
}

// delete removes one snapshot AND the storage behind it, and reports whether it did.
// The snapshot class is expected to use deletionPolicy Retain, so that deleting the
// gofer namespace (or a snapshot object by accident) keeps the stored snapshot.
// Pruning is a deliberate deletion, so it first switches the snapshot's
// VolumeSnapshotContent to Delete; deleting the VolumeSnapshot then removes the
// content and the stored snapshot with it.
//
// It works from a fresh read of the snapshot, and leaves it alone if it is gone, if it
// is already being deleted (whoever deleted it chose what happens to its storage, and
// that choice stands), or if still is non-nil and rejects the fresh copy.
func (s *Snapshotter) delete(ctx context.Context, name string, still func(volumeSnapshot) bool) (bool, error) {
	var v volumeSnapshot
	if err := s.Kube.do(ctx, "GET", s.path(name), "", nil, &v); err != nil {
		if IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("snapshot: read %s before deleting it: %w", name, err)
	}
	if v.terminating() || (still != nil && !still(v)) {
		return false, nil
	}
	content, err := s.contentOf(ctx, v)
	if err != nil {
		return false, err
	}
	if content != "" {
		patch := map[string]any{"spec": map[string]any{"deletionPolicy": "Delete"}}
		err := s.Kube.do(ctx, "PATCH", snapshotAPI+"/volumesnapshotcontents/"+url.PathEscape(content), "application/merge-patch+json", patch, nil)
		if err != nil && !IsNotFound(err) {
			return false, fmt.Errorf("snapshot: set %s to delete its storage: %w", content, err)
		}
	}
	if err := s.Kube.do(ctx, "DELETE", s.path(name), "", nil, nil); err != nil {
		if IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("snapshot: delete %s: %w", name, err)
	}
	return true, nil
}

// contentOf names the VolumeSnapshotContent holding a snapshot's storage, or "" if it
// has none. Normally the snapshot's status names it. The controller can create the
// content and fail to record its name in the snapshot's status, which is exactly the
// kind of snapshot that fails; then the content is found by the name the controller
// gives a dynamically created one, snapcontent-<snapshot UID>, and used only if it
// refers back to this snapshot.
func (s *Snapshotter) contentOf(ctx context.Context, v volumeSnapshot) (string, error) {
	if v.Status != nil && v.Status.BoundVolumeSnapshotContentName != "" {
		return v.Status.BoundVolumeSnapshotContentName, nil
	}
	if v.Metadata.UID == "" {
		return "", nil
	}
	name := "snapcontent-" + v.Metadata.UID
	var c volumeSnapshotContent
	if err := s.Kube.do(ctx, "GET", snapshotAPI+"/volumesnapshotcontents/"+url.PathEscape(name), "", nil, &c); err != nil {
		if IsNotFound(err) {
			return "", nil
		}
		// Unsure whether storage exists: deleting the snapshot now could strand it.
		return "", fmt.Errorf("snapshot: look for %s's content %s: %w", v.Metadata.Name, name, err)
	}
	ref := c.Spec.VolumeSnapshotRef
	if ref.UID != v.Metadata.UID || ref.Name != v.Metadata.Name || ref.Namespace != s.Namespace {
		return "", nil
	}
	return name, nil
}

// Run is one scheduled backup: take a snapshot, wait up to wait for it to be usable,
// then prune. A snapshot that is not usable in time fails the run. Such a run does
// not Prune, so a broken backup never deletes good ones; it only deletes failed
// snapshots, which are never usable:
//
//   - If the new snapshot was still reporting an error when the wait ended, it is
//     deleted, with its stored content, so it does not linger and block a restore. An
//     error alone does not end the wait: the controller retries, and the snapshot may
//     yet become ready.
//   - If it is merely not ready in time (no error), it is left in place. A slow
//     snapshot (a first full copy of a large volume, a busy storage backend) may still
//     complete and be a good backup, and deleting it would throw that away. If it
//     fails later, the next run deletes it as failed.
//   - Either way, and also if the snapshot could not even be created (a failed one
//     may be what fills the namespace's snapshot quota), any earlier managed snapshot
//     that has failed is deleted too.
//
// If ctx itself ends (the job is being stopped), nothing is cleaned up.
func (s *Snapshotter) Run(ctx context.Context, wait time.Duration) error {
	name, err := s.Take(ctx)
	if err != nil {
		return errors.Join(err, s.afterFailure(ctx, err))
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

// afterFailure cleans up after a run that produced no usable snapshot, given the
// error that ended it: it deletes the run's snapshot if it was still failing, then any
// other snapshot that has failed.
func (s *Snapshotter) afterFailure(ctx context.Context, runErr error) error {
	if ctx.Err() != nil {
		return nil
	}
	var errs []error
	var failing *FailedError
	if errors.As(runErr, &failing) {
		// It has had the whole wait to recover; delete it unless it has since.
		gone, err := s.delete(ctx, failing.Name, func(v volumeSnapshot) bool { return v.erroring() != "" })
		switch {
		case err != nil:
			errs = append(errs, err)
		case gone:
			s.log().Info("failed snapshot deleted", "snapshot", failing.Name, "error", failing.Message)
		}
	}
	if _, err := s.pruneFailed(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
