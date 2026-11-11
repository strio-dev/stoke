package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRejectsJobsFlagBeforeLoadingOrExecution(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"run", "--jobs", "4", "-f", "/missing/workflow.yaml", "--coopr-binary", "/missing/coopr"}, &out, &errOut)
	if code != 2 || out.Len() != 0 || !strings.Contains(errOut.String(), "flag provided but not defined: -jobs") {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, &out, &errOut)
	}
}
