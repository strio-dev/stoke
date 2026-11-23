package workflow

import (
	"strings"
	"testing"
)

func TestJobNameFitsRuntimeOutputDirectory(t *testing.T) {
	// Ordinary direct task names retain the full filesystem component limit.
	name := strings.Repeat("a", MaxJobNameBytes)
	w, err := Decode(strings.NewReader("version: 1\ntasks: {" + name + ": {image: scratch}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Expand(nil); err != nil {
		t.Fatalf("valid direct name rejected: %v", err)
	}
	if _, err := Decode(strings.NewReader("version: 1\ntasks: {" + name + "a: {image: scratch}}\n")); err == nil {
		t.Fatal("oversized direct name admitted")
	}
	w, err = Decode(strings.NewReader("version: 1\ntasks: {" + name + ": {image: scratch, strategy: {matrix: {mode: [one]}}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Expand(nil); err == nil || !strings.Contains(err.Error(), "expanded job name") {
		t.Fatalf("oversized variant not rejected during expansion: %v", err)
	}
}
