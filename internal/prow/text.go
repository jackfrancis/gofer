package prow

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	binarySniffBytes = 8000
	// maxScanLine is the longest line kept whole for search and read; the rest of an
	// over-long line is dropped (it is still counted as one line).
	maxScanLine = 64 << 10
)

// isBinary reports a NUL byte near the start, the usual sign of non-text content.
func isBinary(head []byte) bool {
	if len(head) > binarySniffBytes {
		head = head[:binarySniffBytes]
	}
	return bytes.IndexByte(head, 0) >= 0
}

// normalize copies a log from r to w with terminal control sequences removed and
// every line ending as "\n": ANSI CSI and OSC sequences are dropped (the test
// framework colours its output), "\r\n" becomes "\n", and a bare "\r" — a
// progress-bar redraw — becomes "\n" so redraws do not merge into one huge line. It
// returns the bytes written.
func normalize(w io.Writer, r io.Reader) (int64, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	bw := bufio.NewWriterSize(w, 64<<10)
	var n int64
	put := func(b byte) error {
		n++
		return bw.WriteByte(b)
	}
	const (
		text = iota
		esc  // after ESC
		csi  // inside ESC [ … final
		osc  // inside ESC ] … BEL or ESC \
		oscEsc
		cr // after a bare CR
	)
	state := text
	for {
		b, err := br.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return n, err
		}
		switch state {
		case cr:
			state = text
			if b == '\n' {
				if err := put('\n'); err != nil {
					return n, err
				}
				continue
			}
			if err := put('\n'); err != nil {
				return n, err
			}
			fallthrough
		case text:
			switch b {
			case 0x1b:
				state = esc
			case '\r':
				state = cr
			default:
				if err := put(b); err != nil {
					return n, err
				}
			}
		case esc:
			switch b {
			case '[':
				state = csi
			case ']':
				state = osc
			default:
				state = text // a two-byte escape: drop both
			}
		case csi:
			if b >= 0x40 && b <= 0x7e {
				state = text
			}
		case osc:
			switch b {
			case 0x07:
				state = text
			case 0x1b:
				state = oscEsc
			}
		case oscEsc:
			state = text // ESC \ ends it; anything else, give up on the sequence
		}
	}
	if state == cr {
		if err := put('\n'); err != nil {
			return n, err
		}
	}
	return n, bw.Flush()
}

// lineReader yields lines without their "\n", truncating a line longer than
// maxScanLine (the remainder is consumed, not returned as further lines).
type lineReader struct {
	br  *bufio.Reader
	buf []byte
	num int
	err error
}

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{br: bufio.NewReaderSize(r, 64<<10)}
}

// next returns the next line and true, or false at the end (or on an error, kept in err).
func (l *lineReader) next() ([]byte, bool) {
	l.buf = l.buf[:0]
	got := false
	for {
		frag, err := l.br.ReadSlice('\n')
		if len(frag) > 0 {
			got = true
			if room := maxScanLine - len(l.buf); room > 0 {
				if len(frag) > room {
					l.buf = append(l.buf, frag[:room]...)
				} else {
					l.buf = append(l.buf, frag...)
				}
			}
		}
		switch err {
		case nil:
			l.num++
			return bytes.TrimSuffix(l.buf, []byte{'\n'}), true
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if got {
				l.num++
				return bytes.TrimSuffix(l.buf, []byte{'\n'}), true
			}
			return nil, false
		default:
			l.err = err
			return nil, false
		}
	}
}

// clipLine bounds a line to max bytes, keeping the window around byte col (a match)
// and never splitting a UTF-8 sequence. Invalid UTF-8 is replaced.
func clipLine(s string, col, max int) string {
	if len(s) > max {
		from := 0
		if col > max/2 {
			from = col - max/3
		}
		to := min(from+max, len(s))
		for from > 0 && from < len(s) && !utf8.RuneStart(s[from]) {
			from++
		}
		if to < from {
			to = from
		}
		for to > from && to < len(s) && !utf8.RuneStart(s[to]) {
			to--
		}
		clipped := s[from:to]
		if from > 0 {
			clipped = "…" + clipped
		}
		if to < len(s) {
			clipped += "…"
		}
		s = clipped
	}
	return strings.ToValidUTF8(s, "�")
}
