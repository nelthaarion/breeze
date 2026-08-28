package generator

// Tests for the marker-block machinery, and in particular for the checksum that
// lets `breeze add` tell a block it wrote from one someone has since edited.
//
// The bug these exist for was a formatting mismatch, not a logic error:
// upsertBlock gofmts the file it writes, gofmt puts a blank line between a
// declaration and the comment that follows it, and so a block read back from
// disk never equalled the freshly built one byte-for-byte. Every comparison
// reported "modified".

import (
	"go/format"
	"os"
	"strings"
	"testing"
)

// gofmtInsertsBlankLineBeforeEndMarker is the exact shape that broke the
// comparison: a func declaration followed by the end marker comment.
const testBlockBody = `func setupThing(app *breeze.Breeze, router *breeze.Router) {
	_ = app
	_ = router
}`

func writeTestBlock(t *testing.T, body string) string {
	t.Helper()
	t.Chdir(t.TempDir())

	if err := upsertBlock(blockRequest{
		FileName:  featuresFileName,
		Initial:   featuresTemplate(),
		Prefix:    featureMarkerPrefix,
		Name:      "thing",
		Body:      body,
		Placement: placeAtEOF,
		Stamp:     true,
	}); err != nil {
		t.Fatalf("upsertBlock: %v", err)
	}

	existing, ok := readBlock(featuresFileName, featureMarkerPrefix, "thing")
	if !ok {
		content, _ := os.ReadFile(featuresFileName)
		t.Fatalf("block not found after writing it:\n%s", content)
	}
	return existing
}

// TestStampedBlockRoundTrips is the regression test for the whole failure.
//
// A block is written, read back, and compared against the same body it was
// generated from. Both answers must be "unchanged" and "pristine" â€” before the
// canonical comparison the first was false, and every second `breeze add`
// therefore errored demanding --force.
func TestStampedBlockRoundTrips(t *testing.T) {
	existing := writeTestBlock(t, testBlockBody)
	start, end := markersFor(featureMarkerPrefix, "thing")

	stored, stamp := splitBlock(existing, start, end)
	if stamp == "" {
		t.Fatalf("no checksum on the start marker:\n%s", existing)
	}

	// The precise thing that defeated a byte comparison.
	if !strings.Contains(existing, "}\n\n"+end) {
		t.Logf("note: gofmt did not insert the blank line this test was written for:\n%q", existing)
	}

	if !sameBlockBody(stored, testBlockBody) {
		t.Errorf("a freshly written block does not compare equal to its own source:\nstored:\n%q\nsource:\n%q",
			stored, testBlockBody)
	}
	if !blockIsPristine(stored, stamp) {
		t.Errorf("a freshly written block is not pristine: stamp %q, recomputed %q", stamp, stampFor(stored))
	}
}

// TestStampSurvivesGofmt covers the other time the file gets reformatted: the
// user running gofmt, or their editor doing it on save. Neither has touched the
// block's meaning, so add must still treat it as untouched.
//
// The mangling below is deliberately worse than anything gofmt produces â€” a
// top-level func pushed off column 0 â€” because the point is that the checksum
// tracks meaning rather than layout. It is then run through format.Source, which
// is what gofmt would do to it.
func TestStampSurvivesGofmt(t *testing.T) {
	writeTestBlock(t, testBlockBody)

	content, err := os.ReadFile(featuresFileName)
	if err != nil {
		t.Fatal(err)
	}
	loosened := strings.Replace(string(content), "func setupThing(", "\n\n    func setupThing(", 1)
	if loosened == string(content) {
		t.Fatal("test body no longer contains the declaration this indents")
	}

	for _, stage := range []struct {
		desc string
		src  string
	}{
		{"whitespace mangled", loosened},
		{"then gofmt over the whole file", gofmt(t, loosened)},
	} {
		if err := os.WriteFile(featuresFileName, []byte(stage.src), 0o644); err != nil {
			t.Fatal(err)
		}

		existing, ok := readBlock(featuresFileName, featureMarkerPrefix, "thing")
		if !ok {
			t.Fatalf("%s: block not found", stage.desc)
		}
		start, end := markersFor(featureMarkerPrefix, "thing")
		stored, stamp := splitBlock(existing, start, end)

		if !blockIsPristine(stored, stamp) {
			t.Errorf("%s: layout-only change made the block look hand-edited", stage.desc)
		}
		if !sameBlockBody(stored, testBlockBody) {
			t.Errorf("%s: layout-only change made the block compare unequal to its source", stage.desc)
		}
	}
}

func gofmt(t *testing.T, src string) string {
	t.Helper()
	out, err := format.Source([]byte(src))
	if err != nil {
		t.Fatalf("gofmt: %v\n%s", err, src)
	}
	return string(out)
}

// TestStampDetectsHandEdit is the case --force exists for. Without a checksum
// this is indistinguishable from a block whose flags changed, which is why add
// used to refuse both.
func TestStampDetectsHandEdit(t *testing.T) {
	writeTestBlock(t, testBlockBody)

	content, err := os.ReadFile(featuresFileName)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(content), "_ = app", "_ = app // mine", 1)
	if edited == string(content) {
		t.Fatal("test body no longer contains the line this edits")
	}
	if err := os.WriteFile(featuresFileName, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	existing, _ := readBlock(featuresFileName, featureMarkerPrefix, "thing")
	start, end := markersFor(featureMarkerPrefix, "thing")
	stored, stamp := splitBlock(existing, start, end)

	if blockIsPristine(stored, stamp) {
		t.Error("an edited block still reports as pristine")
	}
}

// TestUnstampedBlockIsNotPristine pins the conservative answer for a block with
// no checksum: unknown provenance means it is not safe to overwrite silently.
func TestUnstampedBlockIsNotPristine(t *testing.T) {
	t.Chdir(t.TempDir())

	if err := upsertBlock(blockRequest{
		FileName:  featuresFileName,
		Initial:   featuresTemplate(),
		Prefix:    featureMarkerPrefix,
		Name:      "thing",
		Body:      testBlockBody,
		Placement: placeAtEOF,
	}); err != nil {
		t.Fatalf("upsertBlock: %v", err)
	}

	existing, ok := readBlock(featuresFileName, featureMarkerPrefix, "thing")
	if !ok {
		t.Fatal("block not found")
	}
	start, end := markersFor(featureMarkerPrefix, "thing")
	stored, stamp := splitBlock(existing, start, end)

	if stamp != "" {
		t.Errorf("a block written without Stamp carries a checksum: %q", stamp)
	}
	if blockIsPristine(stored, stamp) {
		t.Error("a block with no checksum reports as pristine")
	}
	// The body must still be readable and comparable â€” that path is shared with
	// the stamped case.
	if !sameBlockBody(stored, testBlockBody) {
		t.Errorf("unstamped block does not compare equal to its source:\n%q", stored)
	}
}

// TestStampedBlockIsReplacedNotDuplicated checks the pattern still matches a
// start marker carrying a checksum. It is matched on the unstamped prefix, so a
// regression here would append a second block rather than replace the first â€”
// two copies of every declaration, and a project that stops compiling.
func TestStampedBlockIsReplacedNotDuplicated(t *testing.T) {
	writeTestBlock(t, testBlockBody)

	second := strings.Replace(testBlockBody, "_ = app", "_ = app\n\t_ = 1", 1)
	if err := upsertBlock(blockRequest{
		FileName:  featuresFileName,
		Initial:   featuresTemplate(),
		Prefix:    featureMarkerPrefix,
		Name:      "thing",
		Body:      second,
		Placement: placeAtEOF,
		Stamp:     true,
	}); err != nil {
		t.Fatalf("second upsertBlock: %v", err)
	}

	content, err := os.ReadFile(featuresFileName)
	if err != nil {
		t.Fatal(err)
	}
	start, _ := markersFor(featureMarkerPrefix, "thing")
	if n := strings.Count(string(content), start); n != 1 {
		t.Errorf("found %d start markers for the block, want 1:\n%s", n, content)
	}
	if n := strings.Count(string(content), "func setupThing("); n != 1 {
		t.Errorf("found %d copies of the declaration, want 1:\n%s", n, content)
	}

	// And the replacement carries the new body's checksum, not the old one's.
	existing, _ := readBlock(featuresFileName, featureMarkerPrefix, "thing")
	_, end := markersFor(featureMarkerPrefix, "thing")
	stored, stamp := splitBlock(existing, start, end)
	if !blockIsPristine(stored, stamp) {
		t.Error("the replaced block carries a stale checksum")
	}
}
