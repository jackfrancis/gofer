// Package checkout materialises a repository's source at one ref as a local,
// read-only tree the conversation tools can search, list and read as a whole. It is
// workload-only (imported by internal/agent, never by gofer core) and knows nothing
// about GitHub or credentials: it is handed a gzip'd tar stream and a directory.
//
// Everything in the archive is untrusted. Unpack therefore writes only regular files
// and directories, never links or devices; refuses any name that is not a plain
// relative path; forces file modes; and enforces hard caps on the compressed size,
// the unpacked size, the entry count and each file's size. All filesystem access —
// during the unpack and afterwards — goes through an os.Root, so even a name that
// slipped past the checks could not resolve outside the checkout directory.
package checkout

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Limits bound one checkout. The zero value of a field means "use the default".
type Limits struct {
	MaxArchiveBytes  int64 // compressed tarball bytes read from the network
	MaxUnpackedBytes int64 // total bytes of regular files written to disk
	MaxEntries       int   // files + directories materialised
	MaxFileBytes     int64 // a single file larger than this is skipped, not written
}

// DefaultLimits fit a large monorepo (kubernetes/kubernetes at a release tag is
// roughly 45 MB compressed, 300 MB and 35k entries unpacked) with headroom, while
// keeping a hostile or simply enormous repository from filling the sandbox's disk.
var DefaultLimits = Limits{
	MaxArchiveBytes:  300 << 20,
	MaxUnpackedBytes: 1 << 30,
	MaxEntries:       150_000,
	MaxFileBytes:     4 << 20,
}

func (l Limits) withDefaults() Limits {
	if l.MaxArchiveBytes <= 0 {
		l.MaxArchiveBytes = DefaultLimits.MaxArchiveBytes
	}
	if l.MaxUnpackedBytes <= 0 {
		l.MaxUnpackedBytes = DefaultLimits.MaxUnpackedBytes
	}
	if l.MaxEntries <= 0 {
		l.MaxEntries = DefaultLimits.MaxEntries
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = DefaultLimits.MaxFileBytes
	}
	return l
}

// ErrTooLarge reports that an archive exceeded a Limit. The message names which one,
// so the tool error tells the model why the repository cannot be checked out.
var ErrTooLarge = errors.New("repository too large to check out")

// maxNameBytes bounds one entry's path; longer names are rejected.
const maxNameBytes = 4096

// Stats describes what an unpack wrote and what it left out.
type Stats struct {
	Commit   string // the commit the archive was cut from, when the archive says
	Files    int
	Dirs     int
	Bytes    int64 // bytes of regular files written
	Symlinks int   // symbolic and hard links, never materialised
	Rejected int   // entries refused: unsafe name, special file, or a collision
	// Oversize maps each file skipped for exceeding MaxFileBytes to its size.
	Oversize map[string]int64
}

// cappedReader fails with ErrTooLarge once more than max bytes have been read.
type cappedReader struct {
	r    io.Reader
	left int64
	what string
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, fmt.Errorf("%w: %s", ErrTooLarge, c.what)
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// Unpack extracts a gzip'd tar stream (a GitHub "tarball" archive) into dir, which
// must exist and be empty. The archive's single top-level directory
// ("owner-repo-sha/") is stripped, so paths in the tree are repository-relative.
//
// It returns ErrTooLarge (wrapped with the limit hit) as soon as a cap is exceeded;
// the caller removes dir. Unsafe or unsupported entries are skipped and counted
// rather than failing the whole checkout: a repository that merely contains a
// symlink is still worth searching.
func Unpack(r io.Reader, dir string, lim Limits) (Stats, error) {
	lim = lim.withDefaults()
	st := Stats{Oversize: map[string]int64{}}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return st, fmt.Errorf("open checkout directory: %w", err)
	}
	defer root.Close()

	gz, err := gzip.NewReader(&cappedReader{r: r, left: lim.MaxArchiveBytes,
		what: fmt.Sprintf("archive exceeds %s compressed", human(lim.MaxArchiveBytes))})
	if err != nil {
		return st, fmt.Errorf("read archive: %w", err)
	}
	defer gz.Close()
	// Bound the DECOMPRESSED stream too: skipped entries (links, oversize files, tar
	// headers) cost no disk but a gzip bomb of them would still burn the run's time.
	tr := tar.NewReader(&cappedReader{r: gz, left: 4 * lim.MaxUnpackedBytes,
		what: fmt.Sprintf("archive expands past %s", human(4*lim.MaxUnpackedBytes))})

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return st, nil
		}
		if err != nil {
			return st, fmt.Errorf("read archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			// git archive records the commit it was cut from as a pax comment.
			if c := hdr.PAXRecords["comment"]; isHex(c) {
				st.Commit = c
			}
			continue
		}
		name, ok := entryName(hdr.Name)
		if !ok {
			if name != "." { // the stripped top-level directory itself is not a rejection
				st.Rejected++
			}
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if st.Files+st.Dirs >= lim.MaxEntries {
				return st, fmt.Errorf("%w: more than %d files and directories", ErrTooLarge, lim.MaxEntries)
			}
			if err := root.MkdirAll(name, 0o755); err != nil {
				st.Rejected++
				continue
			}
			st.Dirs++
		case tar.TypeReg:
			if hdr.Size > lim.MaxFileBytes {
				st.Oversize[name] = hdr.Size
				continue
			}
			if st.Files+st.Dirs >= lim.MaxEntries {
				return st, fmt.Errorf("%w: more than %d files and directories", ErrTooLarge, lim.MaxEntries)
			}
			if st.Bytes+hdr.Size > lim.MaxUnpackedBytes {
				return st, fmt.Errorf("%w: source exceeds %s unpacked", ErrTooLarge, human(lim.MaxUnpackedBytes))
			}
			n, err := writeFile(root, name, tr, hdr.Size)
			st.Bytes += n
			if err != nil {
				if errors.Is(err, ErrTooLarge) || errors.Is(err, errDisk) {
					return st, err
				}
				st.Rejected++
				continue
			}
			st.Files++
		case tar.TypeSymlink, tar.TypeLink:
			// Never materialised: a link is the classic way out of an extraction root,
			// and nothing the tools do needs one. The tree therefore contains no links
			// at all, so walking it can never be led anywhere.
			st.Symlinks++
		default:
			st.Rejected++ // devices, FIFOs, and anything else that is not source
		}
	}
}

// errDisk marks a write failure of the scratch disk itself (full, read-only), which
// fails the checkout rather than being counted as one bad entry.
var errDisk = errors.New("write checkout")

// writeFile creates name (and its parents) under root and copies exactly size bytes
// into it. The file must not already exist: an archive that names a path twice, or
// names a file where a directory already is, has that entry rejected.
func writeFile(root *os.Root, name string, r io.Reader, size int64) (int64, error) {
	if parent := path.Dir(name); parent != "." {
		if err := root.MkdirAll(parent, 0o755); err != nil {
			return 0, err
		}
	}
	// 0o644 regardless of the archive's mode: nothing unpacked is ever executable.
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, err
	}
	n, err := io.CopyN(f, r, size)
	cerr := f.Close()
	switch {
	case errors.Is(err, ErrTooLarge):
		return n, err
	case err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF):
		// Distinguish a failing disk from a failing archive: a tar/gzip error is
		// surfaced by the next tr.Next(); a write error is the disk.
		var pe *os.PathError
		if errors.As(err, &pe) {
			return n, fmt.Errorf("%w: %v", errDisk, pe.Err)
		}
		return n, err
	case cerr != nil:
		return n, fmt.Errorf("%w: %v", errDisk, cerr)
	}
	return n, nil
}

// entryName maps an archive entry's name to a repository-relative path: the single
// top-level directory is stripped and the rest must be a clean, local, relative
// path. It returns (".", false) for the top-level directory itself and ("", false)
// for anything unsafe — absolute, containing "..", empty, over-long, or carrying a
// NUL or backslash (which would be a separator on some platforms).
func entryName(raw string) (string, bool) {
	if raw == "" || len(raw) > maxNameBytes || strings.ContainsAny(raw, "\x00\\") {
		return "", false
	}
	if strings.HasPrefix(raw, "/") {
		return "", false
	}
	// Check the name as written, before any cleaning could fold a ".." away.
	for _, seg := range strings.Split(strings.TrimSuffix(raw, "/"), "/") {
		if seg == ".." || seg == "" {
			return "", false
		}
	}
	_, rest, found := strings.Cut(path.Clean(raw), "/")
	if !found {
		return ".", false // the top-level "owner-repo-sha" directory (or a stray top-level file)
	}
	if !filepath.IsLocal(filepath.FromSlash(rest)) {
		return "", false
	}
	return rest, true
}

func isHex(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// human renders a byte count for an error message or a tool result.
func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// Human is human, exported for the tool layer's summaries.
func Human(n int64) string { return human(n) }
