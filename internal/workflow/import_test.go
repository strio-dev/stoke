package workflow

import (
	"strings"
	"testing"
)

func TestLocalImportSyntax(t *testing.T) {
	for _, tc := range []struct {
		reference string
		valid     bool
	}{
		{"./shared.yaml", true}, {"./../shared.yaml", true},
		{"registry.test/shared@sha256:" + strings.Repeat("a", 64), true},
		{"shared.yaml", false}, {"../shared.yaml", false}, {"/shared.yaml", false},
		{"./", false}, {"./bad\\path.yaml", false}, {"file://./shared.yaml", false},
		{"registry.test/shared:latest", false},
	} {
		if err := ValidateImportReference(tc.reference); (err == nil) != tc.valid {
			t.Errorf("import %q: valid=%t err=%v", tc.reference, tc.valid, err)
		}
	}
}
