package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootInputFlags(t *testing.T) {
	file := filepath.Join(t.TempDir(), "stoke.yaml")
	if err := os.WriteFile(file, []byte("version: 1\ninputs: {version: {required: true}, mode: {default: release}}\ntasks: {build: {image: scratch, env: {VERSION: {input: version}, MODE: {input: mode}}}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		code int
		want string
	}{
		{"check", []string{"check", "--input", "version=a=b", "-f", file}, 0, "Valid workflow"},
		{"plan", []string{"plan", "--input", "version=", "--input", "mode=123", "-f", file}, 0, "build"},
		{"missing", []string{"run", "-f", file, "--coopr-binary", "/does/not/exist"}, 1, "required input"},
		{"unknown", []string{"plan", "--input", "typo=bad", "-f", file}, 1, "unknown input"},
		{"duplicate", []string{"plan", "--input", "version=a", "--input", "version=b", "-f", file}, 2, "duplicate input"},
		{"malformed", []string{"check", "--input", "version", "-f", file}, 2, "NAME=VALUE"},
		{"empty-name", []string{"check", "--input", "=value", "-f", file}, 2, "NAME=VALUE"},
		{"publish", []string{"publish", "--input", "version=a", "-f", file}, 2, "flag provided but not defined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := run(tc.args, &out, &errOut)
			if code != tc.code || !strings.Contains(out.String()+errOut.String(), tc.want) {
				t.Fatalf("code=%d out=%s err=%s", code, &out, &errOut)
			}
		})
	}
}
