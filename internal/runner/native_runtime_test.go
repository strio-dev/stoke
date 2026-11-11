package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Native network/platform tests accept absolute executable overrides through
// STOKE_NETWORK_RUNTIME/STOKE_PLATFORM_RUNTIME, respectively. Otherwise they use
// crun from PATH, just like the CLI, without falling back after an invalid override.
func nativeTestRuntime(override string) (string, error) {
	selected := os.Getenv(override)
	if selected == "" {
		var err error
		selected, err = exec.LookPath("crun")
		if err != nil {
			return "", fmt.Errorf("resolve crun from PATH or set %s: %w", override, err)
		}
	} else if !filepath.IsAbs(selected) {
		return "", fmt.Errorf("%s must select an absolute executable runtime path", override)
	}
	selected, err := filepath.Abs(selected)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(selected)
	if err != nil {
		return "", fmt.Errorf("runtime selected by %s or PATH: %w", override, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("runtime selected by %s or PATH is not an executable regular file: %s", override, selected)
	}
	return selected, nil
}

func TestNativeTestRuntime(t *testing.T) {
	for _, mode := range []string{"explicit", "PATH", "missing PATH", "missing override", "nonexecutable override", "nonexecutable PATH", "relative override", "directory override"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			binary := filepath.Join(dir, "crun")
			if err := os.WriteFile(binary, []byte("fixture, never executed"), 0700); err != nil {
				t.Fatal(err)
			}
			const setting = "STOKE_PLATFORM_RUNTIME"
			t.Setenv("PATH", dir)
			t.Setenv(setting, "")
			switch mode {
			case "explicit":
				t.Setenv(setting, binary)
				t.Setenv("PATH", "")
			case "missing PATH":
				t.Setenv("PATH", filepath.Join(dir, "missing"))
			case "missing override":
				t.Setenv(setting, filepath.Join(dir, "missing"))
			case "nonexecutable override", "nonexecutable PATH":
				if err := os.Chmod(binary, 0600); err != nil {
					t.Fatal(err)
				}
				if mode == "nonexecutable override" {
					t.Setenv(setting, binary)
				}
			case "relative override":
				t.Setenv(setting, "crun")
			case "directory override":
				t.Setenv(setting, dir)
			}
			got, err := nativeTestRuntime(setting)
			if mode == "explicit" || mode == "PATH" {
				if err != nil || got != binary {
					t.Fatalf("got %q, %v; want %q", got, err, binary)
				}
			} else if err == nil || got != "" {
				t.Fatalf("invalid runtime accepted: %q, %v", got, err)
			}
		})
	}
}
