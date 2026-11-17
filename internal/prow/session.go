package prow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Limits bound what one run (one conversation turn) may fetch.
type Limits struct {
	MaxObjectBytes int64         // one object, decompressed
	MaxRunBytes    int64         // every object fetched this run, together
	MaxObjects     int           // objects fetched this run (cache hits are free)
	MaxJobs        int           // distinct job runs opened this run
	MaxListPages   int           // listing pages fetched this run
	FetchTimeout   time.Duration // one object or listing page
	TimeReserve    time.Duration // the slice of the run's remaining time a fetch never spends; negative for none
}

// DefaultLimits fit a large end-to-end job (the CAPZ e2e jobs hold ~700 objects and
// ~55 MiB, the largest a 13 MiB controller log) with headroom.
var DefaultLimits = Limits{
	MaxObjectBytes: 64 << 20,
	MaxRunBytes:    512 << 20,
	MaxObjects:     60,
	MaxJobs:        6,
	MaxListPages:   12,
	FetchTimeout:   60 * time.Second,
	TimeReserve:    2 * time.Minute,
}

// Session is one run's view of Prow artifacts: the jobs opened, and every object and
// listing fetched, cached in a temporary directory until Close. Objects are stored
// under counter names (never their bucket names), through an os.Root, so nothing
// fetched can choose where it lands on disk.
type Session struct {
	c   *Client
	lim Limits

	mu         sync.Mutex
	dir        string
	root       *os.Root
	jobs       []Job
	files      map[string]*File // by bucket/object
	lists      map[string]listing
	downloaded int64
	fetched    int
	listPages  int
	seq        int
}

// File is one fetched object, stored normalised for text (see normalize).
type File struct {
	Object string // the bucket object name
	Rel    string // path inside its job
	Size   int64  // bytes stored
	Binary bool
	local  string
	err    error
}

type listing struct {
	objects    []Object
	prefixes   []string
	incomplete bool
	err        error
}

// NewSession starts a run's session. A zero Limits field uses the default.
func NewSession(c *Client, lim Limits) *Session {
	d := DefaultLimits
	if lim.MaxObjectBytes <= 0 {
		lim.MaxObjectBytes = d.MaxObjectBytes
	}
	if lim.MaxRunBytes <= 0 {
		lim.MaxRunBytes = d.MaxRunBytes
	}
	if lim.MaxObjects <= 0 {
		lim.MaxObjects = d.MaxObjects
	}
	if lim.MaxJobs <= 0 {
		lim.MaxJobs = d.MaxJobs
	}
	if lim.MaxListPages <= 0 {
		lim.MaxListPages = d.MaxListPages
	}
	if lim.FetchTimeout <= 0 {
		lim.FetchTimeout = d.FetchTimeout
	}
	switch {
	case lim.TimeReserve == 0:
		lim.TimeReserve = d.TimeReserve
	case lim.TimeReserve < 0: // explicitly none
		lim.TimeReserve = 0
	}
	return &Session{c: c, lim: lim, files: map[string]*File{}, lists: map[string]listing{}}
}

// Close deletes everything fetched.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root != nil {
		_ = s.root.Close()
		s.root = nil
	}
	if s.dir != "" {
		_ = os.RemoveAll(s.dir)
		s.dir = ""
	}
	s.files = map[string]*File{}
	s.lists = map[string]listing{}
}

// Open parses a job URL and registers the job, enforcing the per-run job limit. It
// returns the job and the path inside it the URL pointed at.
func (s *Session) Open(raw string) (Job, string, error) {
	job, rel, err := ParseJobURL(raw)
	if err != nil {
		return Job{}, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j == job {
			return job, rel, nil
		}
	}
	if len(s.jobs) >= s.lim.MaxJobs {
		names := make([]string, len(s.jobs))
		for i, j := range s.jobs {
			names[i] = j.Name() + "/" + j.BuildID()
		}
		return Job{}, "", fmt.Errorf("job %w: at most %d CI jobs per answer (already opened %s)", ErrLimit, s.lim.MaxJobs, strings.Join(names, ", "))
	}
	s.jobs = append(s.jobs, job)
	return job, rel, nil
}

// runDeadlineKey holds the run's deadline once an operation has narrowed ctx with a
// time budget of its own (see withBudget), so the reserve is still measured from the
// run's end and not from the budget's.
type runDeadlineKey struct{}

type runDeadline struct {
	at time.Time
	ok bool
}

// withBudget bounds ctx to d for one operation (a search), remembering the run's
// deadline. A budget shorter than the reserve must not be read as a run that is out
// of time.
func withBudget(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, set := ctx.Value(runDeadlineKey{}).(runDeadline); !set {
		at, ok := ctx.Deadline()
		ctx = context.WithValue(ctx, runDeadlineKey{}, runDeadline{at, ok})
	}
	return context.WithTimeout(ctx, d)
}

// timeout returns how long the next network call may take, or an error when the run
// has no time left to spend on one. The reserve comes off the run's deadline; an
// operation's own budget (withBudget) caps the call but is not reserved against.
func (s *Session) timeout(ctx context.Context) (time.Duration, error) {
	t := s.lim.FetchTimeout
	run, set := ctx.Value(runDeadlineKey{}).(runDeadline)
	if !set {
		run.at, run.ok = ctx.Deadline()
	}
	if run.ok {
		if left := time.Until(run.at) - s.lim.TimeReserve; left < t {
			t = left
		}
	}
	if dl, ok := ctx.Deadline(); ok && set {
		if left := time.Until(dl); left < t {
			t = left
		}
	}
	if t <= 0 {
		return 0, fmt.Errorf("not enough time left in this run to fetch CI logs; answer from what you have")
	}
	return t, nil
}

// ensureDir creates the session's scratch directory on first use. Caller holds mu.
func (s *Session) ensureDir() error {
	if s.root != nil {
		return nil
	}
	dir, err := os.MkdirTemp("", "gofer-ci-")
	if err != nil {
		return fmt.Errorf("this runtime has no writable scratch space for CI logs")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("this runtime has no writable scratch space for CI logs")
	}
	s.dir, s.root = dir, root
	return nil
}

// Fetch returns rel (a path inside job) fetched and stored, downloading it on first
// use. A failure is remembered too, so a retry does not download again.
func (s *Session) Fetch(ctx context.Context, job Job, rel string) (*File, error) {
	object, err := job.Object(rel)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(object, "/") {
		return nil, fmt.Errorf("a file path is required")
	}
	key := job.Bucket + "/" + object
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.files[key]; ok {
		return f, f.err
	}
	if s.fetched >= s.lim.MaxObjects {
		return nil, fmt.Errorf("file %w: at most %d CI files fetched per answer; use what is already fetched", ErrLimit, s.lim.MaxObjects)
	}
	left := s.lim.MaxRunBytes - s.downloaded
	if left <= 0 {
		return nil, fmt.Errorf("%w: this answer's CI log space (%s) is used up", ErrTooLarge, human(s.lim.MaxRunBytes))
	}
	t, err := s.timeout(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.ensureDir(); err != nil {
		return nil, err
	}
	max := min(s.lim.MaxObjectBytes, left)
	cctx, cancel := context.WithTimeout(ctx, t)
	defer cancel()

	cleanRel, _ := CleanRel(rel)
	f := &File{Object: object, Rel: cleanRel}
	s.fetched++
	f.err = s.download(cctx, job.Bucket, f, max)
	if f.err != nil && cctx.Err() != nil && ctx.Err() == nil {
		f.err = fmt.Errorf("fetching %s timed out", cleanRel)
	}
	s.files[key] = f
	return f, f.err
}

// download fetches one object to a raw file, then stores it normalised (text) or as
// is (binary). Caller holds mu.
func (s *Session) download(ctx context.Context, bucket string, f *File, max int64) error {
	s.seq++
	rawName := strconv.Itoa(s.seq) + ".raw"
	raw, err := s.root.OpenFile(rawName, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("this runtime has no writable scratch space for CI logs")
	}
	defer func() {
		_ = raw.Close()
		_ = s.root.Remove(rawName)
	}()
	n, err := s.c.Fetch(ctx, bucket, f.Object, raw, max)
	s.downloaded += n
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return notFoundError(f.Rel)
		}
		return err
	}
	head := make([]byte, binarySniffBytes)
	hn, _ := raw.ReadAt(head, 0)
	local := strconv.Itoa(s.seq)
	if isBinary(head[:hn]) {
		f.Binary = true
		f.Size = n
		return nil // never searched or read; nothing to keep
	}
	out, err := s.root.OpenFile(local, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("this runtime has no writable scratch space for CI logs")
	}
	if _, err := raw.Seek(0, io.SeekStart); err != nil {
		_ = out.Close()
		return err
	}
	size, err := normalize(out, raw)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = s.root.Remove(local)
		return err
	}
	f.Size, f.local = size, local
	return nil
}

// open opens a fetched text file for reading.
func (s *Session) open(f *File) (*os.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root == nil || f.local == "" {
		return nil, fmt.Errorf("%s is not available", f.Rel)
	}
	return s.root.Open(f.local)
}

// ErrLimit reports a per-run limit reached.
var ErrLimit = errors.New("limit reached")

// notFoundError is a path missing from a job; it matches ErrNotFound.
type notFoundError string

func (e notFoundError) Error() string        { return string(e) + " does not exist in this job" }
func (e notFoundError) Is(target error) bool { return target == ErrNotFound }

// Entry is one listed object or directory, relative to the job.
type Entry struct {
	Rel  string // path inside the job; directories end in "/"
	Dir  bool
	Size int64
}

// List lists the job directory rel. With recursive false it returns rel's immediate
// children (files with sizes, subdirectories); with recursive true every object
// under rel. One listing page (1,000 objects) at most: incomplete reports more.
func (s *Session) List(ctx context.Context, job Job, rel string, recursive bool) (entries []Entry, incomplete bool, err error) {
	prefix, err := job.Object(rel)
	if err != nil {
		return nil, false, err
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	key := job.Bucket + "/" + prefix + "|" + strconv.FormatBool(recursive)
	s.mu.Lock()
	l, ok := s.lists[key]
	if !ok {
		if s.listPages >= s.lim.MaxListPages {
			s.mu.Unlock()
			return nil, false, fmt.Errorf("listing %w: at most %d CI listings per answer", ErrLimit, s.lim.MaxListPages)
		}
		t, terr := s.timeout(ctx)
		if terr != nil {
			s.mu.Unlock()
			return nil, false, terr
		}
		s.listPages++
		cctx, cancel := context.WithTimeout(ctx, t)
		page, lerr := s.c.List(cctx, job.Bucket, prefix, !recursive, "")
		cancel()
		l = listing{objects: page.Objects, prefixes: page.Prefixes, incomplete: page.Next != "", err: lerr}
		s.lists[key] = l
	}
	s.mu.Unlock()
	if l.err != nil {
		return nil, false, l.err
	}
	base := job.Path + "/"
	for _, p := range l.prefixes {
		if r := strings.TrimPrefix(p, base); r != p && r != "" {
			entries = append(entries, Entry{Rel: r, Dir: true})
		}
	}
	for _, o := range l.objects {
		r := strings.TrimPrefix(o.Name, base)
		if r == o.Name || r == "" || strings.HasSuffix(r, "/") {
			continue // outside the job (never, by the prefix) or a directory placeholder
		}
		entries = append(entries, Entry{Rel: r, Size: o.Size})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		return entries[i].Rel < entries[j].Rel
	})
	return entries, l.incomplete, nil
}
