package db

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncatePURLLeavesRealValuesAlone(t *testing.T) {
	const purl = "pkg:maven/org.eclipse.jetty/jetty-server@12.0.16"
	got, cut := TruncatePURL(purl)
	if cut || got != purl {
		t.Fatalf("TruncatePURL(%q) = %q, %v; want unchanged", purl, got, cut)
	}
}

// A generated snapshot once reported a multi-kilobyte "purl", which Postgres
// refused to index on idx_sbom_packages_p_url and so aborted the whole
// dependency transaction. Seen at both of its ceilings: "index row requires
// 32344 bytes, maximum size is 8191" and "index row size 5616 exceeds btree
// version 4 maximum 2704".
func TestTruncatePURLBoundsOverlongValue(t *testing.T) {
	got, cut := TruncatePURL("pkg:npm/" + strings.Repeat("a", 32000))
	if !cut {
		t.Fatal("TruncatePURL reported no truncation for a 32kB value")
	}
	if len(got) > purlIndexMaxLen {
		t.Fatalf("len = %d, want <= %d", len(got), purlIndexMaxLen)
	}
}

// Postgres rejects a text value that is not valid UTF-8, so the cut has to
// land on a rune boundary rather than mid-sequence.
func TestTruncatePURLCutsOnRuneBoundary(t *testing.T) {
	// "…" is three bytes, so a byte-wise cut at the cap splits one.
	got, cut := TruncatePURL("pkg:generic/" + strings.Repeat("…", 2000))
	if !cut {
		t.Fatal("TruncatePURL reported no truncation")
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncated value is not valid UTF-8: %q", got[len(got)-8:])
	}
}

// The cap must clear BTMaxItemSize (a third of an 8kB page), not the louder
// 8191 ceiling: a 5616-byte purl failed on 2704 alone. Guarded so raising
// purlIndexMaxLen toward 8191 — which the first error message invites — fails
// here rather than back in production.
func TestPURLCapClearsBtreeItemLimit(t *testing.T) {
	const btMaxItemSize = 2704
	if purlIndexMaxLen >= btMaxItemSize {
		t.Fatalf("purlIndexMaxLen = %d, must be < %d (Postgres BTMaxItemSize)",
			purlIndexMaxLen, btMaxItemSize)
	}
}
