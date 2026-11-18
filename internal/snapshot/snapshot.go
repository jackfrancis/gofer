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
	"net/http"
	"net/url"
	"slices"
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

// writeTimeout and recoveryTimeout bound a deletion once it is under way: up to
// writeTimeout to make its writes (switch the content, delete the snapshot), then, if
// those did not go through, a fresh recoveryTimeout to read back what happened and
// undo. Both run to the end even if the job is being stopped, so nothing is left half
// done, and together they fit in a pod's default 30s grace period. Variables so tests
// can shorten them.
var (
	writeTimeout    = 10 * time.Second
	recoveryTimeout = 10 * time.Second
)

// MaxErrorAge bounds how long an erroring snapshot is kept, counted from its creation
// rather than from its error. The controller restarts an error's clock whenever the
// message changes, so a driver whose messages differ per attempt (a request ID, say)
// would otherwise never let one age past the grace period.
const MaxErrorAge = 24 * time.Hour

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
		ResourceVersion   string            `json:"resourceVersion,omitempty"`
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
		ReadyToUse                     *bool      `json:"readyToUse,omitempty"`
		CreationTime                   *time.Time `json:"creationTime,omitempty"`
		BoundVolumeSnapshotContentName string     `json:"boundVolumeSnapshotContentName,omitempty"`
		RestoreSize                    string     `json:"restoreSize,omitempty"`
		Error                          *struct {
			Message string     `json:"message,omitempty"`
			Time    *time.Time `json:"time,omitempty"`
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

// taken reports whether the storage system has cut the snapshot: the controller sets
// status.creationTime once CreateSnapshot succeeds. A driver that copies the data in
// the background reports success before the snapshot is ready, and the controller
// keeps the error from an earlier attempt until it is ready, so a taken snapshot that
// still shows an error is making progress, not failing.
func (v volumeSnapshot) taken() bool { return v.Status != nil && v.Status.CreationTime != nil }

// managedBy reports whether the snapshot carries this package's labels for claim.
func (v volumeSnapshot) managedBy(claim string) bool {
	return v.Metadata.Labels[LabelManagedBy] == ManagedBy && v.Metadata.Labels[LabelClaim] == claim
}

// terminating reports whether the snapshot is already being deleted.
func (v volumeSnapshot) terminating() bool { return v.Metadata.DeletionTimestamp != nil }

// failed reports whether a snapshot has given up: it reports an error, and that error
// has lasted the grace period, so the controller's retries have had that long to
// succeed. The error's age comes from status.error.time, which the controller keeps
// while the message stays the same (the snapshot's creation if it is missing or
// earlier). A snapshot still erroring MaxErrorAge after its creation has failed too.
//
// A snapshot the storage system has already taken (see taken) does not count as
// failed, whatever error it still shows, as long as it still has its own content: it
// is treated like any slow snapshot. Deciding that needs no comparison of
// status.creationTime (the storage system's clock) with the error's time (the
// controller's), and it errs towards keeping a backup. A taken snapshot whose content
// has gone, or belongs to another snapshot, has failed: the controller marks a once
// ready snapshot unready in that case but leaves its creationTime, and there is no
// storage of its own left to lose. If its content cannot be read, it is kept.
func (s *Snapshotter) failed(ctx context.Context, v volumeSnapshot) bool {
	if !s.errorOutlasted(v) {
		return false
	}
	if !v.taken() {
		return true
	}
	c, err := s.contentOf(ctx, v)
	if err != nil {
		s.log().Warn("cannot tell whether an erroring snapshot still has its content; keeping it", "snapshot", v.Metadata.Name, "err", err)
		return false
	}
	return s.failedWith(v, c)
}

// failedWith is failed for a snapshot whose own content (or nil, if it has none) has
// already been looked up, so a deletion decides on the same content it then acts on.
func (s *Snapshotter) failedWith(v volumeSnapshot, c *ownedContent) bool {
	return s.errorOutlasted(v) && (!v.taken() || c == nil)
}

// errorOutlasted reports whether the snapshot reports an error that has lasted the
// grace period, or is still erroring MaxErrorAge after its creation.
func (s *Snapshotter) errorOutlasted(v volumeSnapshot) bool {
	if v.erroring() == "" {
		return false
	}
	created := v.Metadata.CreationTimestamp
	since := created
	if t := v.Status.Error.Time; t != nil && t.After(since) {
		since = *t
	}
	now := s.now()
	return now.Sub(since) >= s.grace() || now.Sub(created) >= max(MaxErrorAge, s.grace())
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
			if v.taken() {
				msg = "" // cut, still copying: slow, not failing (see taken)
			}
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
//   - A failed snapshot (one whose error has outlasted the grace period, see
//     failed) goes wherever it sits: it is not a backup, and while it
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
		var still func(volumeSnapshot, *ownedContent) bool // re-checked on each fresh read before deleting
		switch {
		case v.terminating():
			continue // already going; neither a backup to count nor one to delete
		case s.failed(ctx, v):
			// Not a backup, and it blocks a restore: delete it wherever it sits, as long
			// as it has not recovered in the meantime.
			still = s.failedWith
		case failedOnly:
			continue
		case usable < keep:
			if v.ready() {
				usable++
			}
			continue
		}
		if err := ctx.Err(); err != nil {
			errs = append(errs, err) // being stopped: start no more deletions
			break
		}
		gone, err := s.delete(ctx, v, still)
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
	Metadata struct {
		UID             string `json:"uid"`
		ResourceVersion string `json:"resourceVersion"`
	} `json:"metadata"`
	Spec struct {
		DeletionPolicy    string `json:"deletionPolicy"`
		VolumeSnapshotRef struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
			UID       string `json:"uid"`
		} `json:"volumeSnapshotRef"`
	} `json:"spec"`
}

// ownedContent is a snapshot's content as checked: its identity, the version that
// was checked, and the deletion policy it had.
type ownedContent struct {
	name, uid, resourceVersion, policy string
}

// delete removes one listed snapshot AND the storage behind it, and reports whether
// it did. The snapshot class is expected to use deletionPolicy Retain, so that
// deleting the gofer namespace (or a snapshot object by accident) keeps the stored
// snapshot. Pruning is a deliberate deletion, so it first switches the snapshot's
// VolumeSnapshotContent to Delete; deleting the VolumeSnapshot then removes the
// content and the stored snapshot with it. (The switch has to come first: the
// controller acts on the policy the content has when the snapshot goes.)
//
// Each attempt works from one consistent view, and every write is conditional on it,
// so nothing that changes in between is touched:
//
//   - The snapshot is read afresh and left alone unless it is still the object that
//     was listed (same UID, still carrying this package's labels for the claim), is
//     not already being deleted (whoever deleted it chose what happens to its
//     storage), and, if still is non-nil, still passes it.
//   - Its content is looked up once (see contentOf), and that same content is what
//     still judges and what is switched; the switch is conditional on the content's
//     UID and resourceVersion as read.
//   - The delete is conditional on the snapshot's UID and resourceVersion as read, so
//     a snapshot that changes in between, say by becoming ready, is not deleted.
//
// The snapshot controller itself changes a snapshot in answer to the switch (it adds a
// bound-protection finalizer once the content's policy is Delete), so a delete that
// meets a conflict starts a new attempt from a fresh read, content lookup included,
// up to deleteAttempts times.
//
// If the deletion is abandoned and the snapshot is not going, every content it
// switched gets its deletion policy back, and the snapshot is left for the next run to
// judge again. If it is going (some other deletion got there first), the contents'
// policies are left as they are: a content's policy is never changed while its
// snapshot is being deleted, in either direction. A write whose outcome is unknown
// (the request failed without a definite answer) is not assumed either way: the
// objects are read again to see what happened before anything is undone, so a backup
// that survives is not left at Delete and one that is going is not switched back to
// Retain, which would strand its storage. Writes run under writeTimeout and the read
// back and undo under a fresh recoveryTimeout, both to the end even if the job is
// being stopped.
func (s *Snapshotter) delete(ctx context.Context, listed volumeSnapshot, still func(volumeSnapshot, *ownedContent) bool) (bool, error) {
	name := listed.Metadata.Name
	var (
		v       volumeSnapshot // the latest read
		c       *ownedContent  // its content as looked up in the latest attempt
		touched []ownedContent // contents a switch was sent for, as they were before it
		wctx    context.Context
		cancelW context.CancelFunc = func() {}
	)
	defer func() { cancelW() }()
	// Until the first write, everything runs on ctx, so a job being stopped starts
	// nothing. From the first write on, it runs to the end on its own budget.
	writeCtx := func() context.Context {
		if wctx == nil {
			wctx, cancelW = context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
		}
		return wctx
	}
	readCtx := func() context.Context {
		if wctx != nil {
			return wctx
		}
		return ctx
	}
	// deleted finishes a deletion that went through.
	deleted := func() (bool, error) {
		if c == nil {
			s.checkLeftBehind(v)
		}
		return true, nil
	}
	// abandon ends a deletion that did not go through, or may not have (err is what
	// stopped it, nil if it was a decision; sent says whether a DELETE was sent), and
	// reports whether the snapshot went after all.
	abandon := func(err error, sent bool) (bool, error) {
		if len(touched) == 0 && !sent {
			return false, err
		}
		if sent && IsNotFound(err) && len(touched) == 0 {
			return false, nil // someone else deleted it first
		}
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recoveryTimeout)
		defer cancel()
		gone, gerr := s.goneSince(rctx, v)
		if gerr != nil {
			if len(touched) == 0 {
				return false, errors.Join(err, fmt.Errorf("snapshot: could not tell whether %s went: %w", name, gerr))
			}
			return false, errors.Join(err, fmt.Errorf("snapshot: could not tell whether %s is going (%w), so its content's deletion policy is as the attempt left it", name, gerr))
		}
		if gone {
			if len(touched) > 0 {
				s.log().Info("snapshot is going; leaving its content's policy as it is", "snapshot", name)
			}
			if sent && err != nil && !refused(err) {
				return deleted() // the lost answer was a success
			}
			return false, nil // someone else's deletion
		}
		errs := []error{err}
		for _, t := range touched {
			errs = append(errs, s.undo(rctx, v, t))
		}
		return false, errors.Join(errs...)
	}
	for attempt := 1; ; attempt++ {
		var cur volumeSnapshot
		if err := s.Kube.do(readCtx(), "GET", s.path(name), "", nil, &cur); err != nil {
			if IsNotFound(err) {
				if len(touched) > 0 {
					return abandon(err, false)
				}
				return false, nil
			}
			return abandon(fmt.Errorf("snapshot: read %s before deleting it: %w", name, err), false)
		}
		v = cur
		switch {
		case v.Metadata.UID != listed.Metadata.UID, !v.managedBy(s.Claim):
			s.log().Info("snapshot is no longer the one listed; leaving it alone", "snapshot", name)
			return abandon(nil, false)
		case v.terminating():
			return abandon(nil, false)
		}
		var err error
		c, err = s.contentOf(readCtx(), v)
		if err != nil {
			return abandon(err, false)
		}
		if still != nil && !still(v, c) {
			return abandon(nil, false)
		}
		if wctx == nil && ctx.Err() != nil {
			return false, ctx.Err() // being stopped, and nothing written yet: start nothing
		}
		if c != nil && c.policy != "Delete" {
			touched = append(touched, *c)
			_, err := s.setPolicy(writeCtx(), c.name, c.uid, c.resourceVersion, "Delete")
			switch {
			case err == nil:
			case IsNotFound(err):
				touched = touched[:len(touched)-1] // the content is gone: nothing switched
				c = nil
			case IsConflict(err):
				touched = touched[:len(touched)-1]
				s.log().Info("content changed since it was checked; leaving the snapshot for the next run", "snapshot", name, "content", c.name)
				return abandon(nil, false)
			case statusCode(err) == http.StatusUnprocessableEntity:
				touched = touched[:len(touched)-1]
				// A different UID under the name (the content was replaced) is refused as a
				// change to an immutable field; so is a change a validation rule or an
				// admission policy rejects. Only the first is benign.
				if replaced, rerr := s.replaced(readCtx(), *c); rerr == nil && replaced {
					s.log().Info("content was replaced since it was checked; leaving the snapshot for the next run", "snapshot", name, "content", c.name)
					return abandon(nil, false)
				}
				return abandon(fmt.Errorf("snapshot: set %s to delete its storage: refused: %w", c.name, err), false)
			case refused(err):
				touched = touched[:len(touched)-1]
				return abandon(fmt.Errorf("snapshot: set %s to delete its storage: %w", c.name, err), false)
			default:
				// It may or may not have been applied; abandon reads back before undoing.
				return abandon(fmt.Errorf("snapshot: set %s to delete its storage: %w", c.name, err), false)
			}
		}
		opts := map[string]any{
			"kind": "DeleteOptions", "apiVersion": "v1",
			"preconditions": map[string]any{"uid": v.Metadata.UID, "resourceVersion": v.Metadata.ResourceVersion},
		}
		err = s.Kube.do(writeCtx(), "DELETE", s.path(name), "application/json", opts, nil)
		if err == nil {
			return deleted()
		}
		if IsConflict(err) && attempt < deleteAttempts {
			select {
			case <-readCtx().Done():
			case <-time.After(time.Duration(attempt) * 100 * time.Millisecond):
			}
			continue
		}
		if IsConflict(err) {
			s.log().Info("snapshot kept changing; leaving it for the next run", "snapshot", name)
			return abandon(nil, false)
		}
		return abandon(fmt.Errorf("snapshot: delete %s: %w", name, err), true)
	}
}

// checkLeftBehind looks, after deleting a snapshot that had no content of its own,
// for a content that turned up for it in the meantime. The controller creates a
// content and records it in the snapshot's status in two separate writes, and a
// binding can be repaired by hand, so one can appear between the last lookup and the
// delete without changing the snapshot. It is left as it is (it may hold a backup
// someone repaired) and logged, so an operator can recover or discard it
// (docs/backup-restore.md). Both names known for it are checked: the one the
// snapshot's status records, and the dynamic one, snapcontent-<snapshot UID>; finding
// contents under other names would need list access to every content in the cluster.
func (s *Snapshotter) checkLeftBehind(v volumeSnapshot) {
	var names []string
	if v.Status != nil && v.Status.BoundVolumeSnapshotContentName != "" {
		names = append(names, v.Status.BoundVolumeSnapshotContentName)
	}
	if v.Metadata.UID != "" && !slices.Contains(names, "snapcontent-"+v.Metadata.UID) {
		names = append(names, "snapcontent-"+v.Metadata.UID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), recoveryTimeout)
	defer cancel()
	for _, name := range names {
		c, err := s.readContent(ctx, name)
		switch {
		case IsNotFound(err):
		case err != nil:
			s.log().Warn("could not check whether a deleted snapshot left a content behind", "snapshot", v.Metadata.Name, "content", name, "err", err)
		case s.owns(c, v):
			s.log().Warn("a content for the deleted snapshot appeared meanwhile and was left behind; to keep or discard it, see 'A content left behind' in docs/backup-restore.md",
				"snapshot", v.Metadata.Name, "content", name, "deletionPolicy", c.Spec.DeletionPolicy)
		}
	}
}

// deleteAttempts bounds how many attempts a deletion makes when its snapshot keeps
// changing under it.
const deleteAttempts = 5

// replaced reports whether a content is gone, or another object now has its name,
// since it was read as c.
func (s *Snapshotter) replaced(ctx context.Context, c ownedContent) (bool, error) {
	cur, err := s.readContent(ctx, c.name)
	if IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return cur.Metadata.UID != c.uid, nil
}

// goneSince reads a snapshot again after a delete that did not go through or whose
// outcome is unknown, and reports whether it is going: it is gone, replaced by
// another object, or being deleted.
func (s *Snapshotter) goneSince(ctx context.Context, v volumeSnapshot) (bool, error) {
	var cur volumeSnapshot
	if err := s.Kube.do(ctx, "GET", s.path(v.Metadata.Name), "", nil, &cur); err != nil {
		if IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	return cur.Metadata.UID != v.Metadata.UID || cur.terminating(), nil
}

// undo gives a snapshot's content back the deletion policy it had, if it still
// belongs to the snapshot and reads Delete, and the snapshot is not going (a content's
// policy is never changed while its snapshot is being deleted). It works from fresh
// reads, conditional on each, and retries a few times, since the write it undoes may
// itself have had an unknown outcome.
func (s *Snapshotter) undo(ctx context.Context, v volumeSnapshot, c ownedContent) error {
	var lastErr error
	for range 3 {
		if gone, err := s.goneSince(ctx, v); err != nil {
			lastErr = err
			continue
		} else if gone {
			s.log().Info("snapshot is already going; leaving its content's policy as it is", "snapshot", v.Metadata.Name)
			return nil
		}
		cur, err := s.readContent(ctx, c.name)
		switch {
		case IsNotFound(err):
			return nil
		case err != nil:
			lastErr = err
			continue
		case cur.Metadata.UID != c.uid || !s.owns(cur, v) || cur.Spec.DeletionPolicy != "Delete":
			return nil
		}
		_, err = s.setPolicy(ctx, c.name, c.uid, cur.Metadata.ResourceVersion, c.policy)
		if err == nil || IsNotFound(err) {
			return nil // set back, or the content is gone
		}
		// Anything else (a conflict, a 422 for a replaced content or a rejected change, an
		// unknown outcome) is judged by the next read.
		lastErr = err
	}
	return fmt.Errorf("snapshot: %s may be left with deletionPolicy Delete; set it back to %s by hand: %w", c.name, c.policy, lastErr)
}

// setPolicy sets a content's deletion policy, conditional on its UID and
// resourceVersion (the API server answers 409 if either has changed), and returns the
// content's new resourceVersion.
func (s *Snapshotter) setPolicy(ctx context.Context, content, uid, resourceVersion, policy string) (string, error) {
	patch := map[string]any{
		"metadata": map[string]any{"uid": uid, "resourceVersion": resourceVersion},
		"spec":     map[string]any{"deletionPolicy": policy},
	}
	var out volumeSnapshotContent
	err := s.Kube.do(ctx, "PATCH", snapshotAPI+"/volumesnapshotcontents/"+url.PathEscape(content), "application/merge-patch+json", patch, &out)
	return out.Metadata.ResourceVersion, err
}

func (s *Snapshotter) readContent(ctx context.Context, name string) (volumeSnapshotContent, error) {
	var c volumeSnapshotContent
	err := s.Kube.do(ctx, "GET", snapshotAPI+"/volumesnapshotcontents/"+url.PathEscape(name), "", nil, &c)
	return c, err
}

// owns reports whether a content refers back to the snapshot by name, namespace and
// UID.
func (s *Snapshotter) owns(c volumeSnapshotContent, v volumeSnapshot) bool {
	ref := c.Spec.VolumeSnapshotRef
	return ref.UID == v.Metadata.UID && ref.Name == v.Metadata.Name && ref.Namespace == s.Namespace
}

// contentOf finds the VolumeSnapshotContent holding a snapshot's storage, or nil if
// the snapshot has none of its own. The content is the one the snapshot's status
// names or, if the controller created a content but never recorded its name (exactly
// the kind of snapshot that fails), the one it gives a dynamically created content:
// snapcontent-<snapshot UID>. Either way it counts only if it refers back to this
// snapshot (see owns): a status can name a content that belongs to another snapshot
// (the controller reports that as SnapshotMisbound), and that content, another
// backup's storage, must not be touched.
func (s *Snapshotter) contentOf(ctx context.Context, v volumeSnapshot) (*ownedContent, error) {
	var name string
	switch {
	case v.Status != nil && v.Status.BoundVolumeSnapshotContentName != "":
		name = v.Status.BoundVolumeSnapshotContentName
	case v.Metadata.UID != "":
		name = "snapcontent-" + v.Metadata.UID
	default:
		return nil, nil
	}
	c, err := s.readContent(ctx, name)
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		// Unsure whether storage exists: deleting the snapshot now could strand it.
		return nil, fmt.Errorf("snapshot: read %s's content %s: %w", v.Metadata.Name, name, err)
	}
	if !s.owns(c, v) {
		ref := c.Spec.VolumeSnapshotRef
		s.log().Warn("content does not belong to the snapshot; leaving it untouched",
			"snapshot", v.Metadata.Name, "content", name, "content_refers_to", ref.Namespace+"/"+ref.Name)
		return nil, nil
	}
	return &ownedContent{name: name, uid: c.Metadata.UID, resourceVersion: c.Metadata.ResourceVersion, policy: c.Spec.DeletionPolicy}, nil
}

// Run is one scheduled backup: take a snapshot, wait up to wait for it to be usable,
// then prune. A snapshot that is not usable in time fails the run. Such a run does
// not Prune, so a broken backup never deletes good ones; it only deletes failed
// snapshots (see failed), which are never usable. That includes the run's own
// snapshot if its error has already lasted the grace period; usually it has not (the
// error appeared during this run's wait), and the next run deletes it if it is still
// failing then. An error alone does not end the wait: the controller retries, and the
// snapshot may yet become ready.
//
// A snapshot that is merely not ready in time (no error) is left in place. A slow
// snapshot (a first full copy of a large volume, a busy storage backend) may still
// complete and be a good backup, and deleting it would throw that away.
//
// Failed snapshots are cleaned up even if the snapshot could not be created: a failed
// one may be what fills the namespace's snapshot quota. If ctx itself ends (the job is
// being stopped), nothing is cleaned up.
func (s *Snapshotter) Run(ctx context.Context, wait time.Duration) error {
	name, err := s.Take(ctx)
	if err != nil {
		return errors.Join(err, s.afterFailure(ctx))
	}
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	size, err := s.Wait(wctx, name)
	if err != nil {
		return errors.Join(err, s.afterFailure(ctx))
	}
	s.log().Info("snapshot ready", "snapshot", name, "restore_size", size)
	_, err = s.Prune(ctx)
	return err
}

// afterFailure cleans up after a run that produced no usable snapshot: it deletes the
// failed snapshots, which cannot touch a usable one.
func (s *Snapshotter) afterFailure(ctx context.Context) error {
	if ctx.Err() != nil {
		return nil
	}
	_, err := s.pruneFailed(ctx)
	return err
}
