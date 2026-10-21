package main

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// macOS's mark looks like a browser's, and never says "the user approved it"
// (flag 0x40), or Gatekeeper would skip its check.
func TestQuarantineTagLooksLikeABrowsers(t *testing.T) {
	got := quarantineTag(time.Unix(0x5f000000, 0))
	if got != "0081;5f000000;Jasqet;" {
		t.Fatalf("quarantineTag = %q", got)
	}
	flags, _ := strconv.ParseUint(strings.SplitN(got, ";", 2)[0], 16, 16)
	if flags&0x40 != 0 || flags&0x01 == 0 {
		t.Fatalf("flags %04x: want 'downloaded' set and 'approved' clear", flags)
	}
}

// The tag goes on before the checks that can take a while (hashing,
// VirusTotal, Defender): the file may already be opened by then.
func TestFilesAreTaggedBeforeTheSlowChecks(t *testing.T) {
	b, err := os.ReadFile("safety.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	start := strings.Index(src, "func (m *Manager) checkSafety(")
	if start < 0 {
		t.Fatal("checkSafety not found")
	}
	body := src[start:]
	body = body[:strings.Index(body, "\n}\n")]
	mark := strings.Index(body, "markFromInternet(")
	if mark < 0 {
		t.Fatal("checkSafety doesn't tag the file")
	}
	for _, slow := range []string{"hashFile(", "m.virusTotal(", "defenderScan(", "checkType("} {
		if i := strings.Index(body, slow); i >= 0 && i < mark {
			t.Errorf("%s runs before the file is tagged", strings.TrimSuffix(slow, "("))
		}
	}
	if strings.Count(body, "markFromInternet(") != 1 {
		t.Error("the file is tagged more than once")
	}
}
