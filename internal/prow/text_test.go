package prow

import (
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"\x1b[38;5;9m\x1b[1mFAIL\x1b[0m\n": "FAIL\n",
		"a\r\nb\n":                         "a\nb\n",
		"10%\r50%\r100%\n":                 "10%\n50%\n100%\n",
		"end\r":                            "end\n",
		"x\x1b]0;title\x07y\n":             "xy\n",
		"x\x1b]8;;http://u\x1b\\link\n":    "xlink\n",
		"two\x1b(Bbyte\n":                  "twoBbyte\n",
		"plain text, no escapes\n":         "plain text, no escapes\n",
		"\r\x1b[2Kprogress\r\x1b[2Kdone\n": "\nprogress\ndone\n",
	} {
		var b strings.Builder
		if _, err := normalize(&b, strings.NewReader(in)); err != nil {
			t.Fatal(err)
		}
		if b.String() != want {
			t.Errorf("normalize(%q) = %q, want %q", in, b.String(), want)
		}
	}
}

func TestLineReaderLongLines(t *testing.T) {
	long := strings.Repeat("x", maxScanLine*3)
	lr := newLineReader(strings.NewReader("a\n" + long + "\nb"))
	var got []int
	for {
		l, ok := lr.next()
		if !ok {
			break
		}
		got = append(got, len(l))
	}
	if len(got) != 3 || got[0] != 1 || got[1] != maxScanLine || got[2] != 1 || lr.num != 3 {
		t.Fatalf("line lengths = %v (num %d); want [1 %d 1]", got, lr.num, maxScanLine)
	}
}

func TestClipLine(t *testing.T) {
	s := strings.Repeat("a", 1000) + "MATCH" + strings.Repeat("b", 1000)
	got := clipLine(s, 1000, 300)
	if !strings.Contains(got, "MATCH") || !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") {
		t.Fatalf("clipLine kept %q", got)
	}
	if got := clipLine("é\xffx", 0, 300); got != "é�x" {
		t.Fatalf("invalid UTF-8 = %q", got)
	}
}
