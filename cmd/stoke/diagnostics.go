package main

import (
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/strio-dev/stoke/internal/runner"
)

// Use the retained report for human diagnostics; stdout remains the JSON result.
func printFailureSummary(out io.Writer, report *runner.Report, targets []string, options runner.Options, nativeCI bool) {
	if report == nil {
		return
	}
	var text strings.Builder
	failed, exports := []string{}, []string{}
	for _, task := range report.Tasks {
		if task.Status == "failed" {
			failed = append(failed, task.Name)
		}
		for _, export := range task.Exports {
			if export.Destination != "" && !slices.Contains(exports, export.Destination) {
				exports = append(exports, export.Destination)
			}
		}
	}
	if len(failed) != 0 {
		fmt.Fprintf(&text, "Failed tasks: %s\n", strings.Join(failed, ", "))
	} else {
		text.WriteString("Workflow failed; inspect the retained report.\n")
	}
	fmt.Fprintf(&text, "Report: %s\n", filepath.Join(report.Directory, "run.json"))
	if nativeCI {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			text.WriteString("Retry with GitHub's Re-run job, preserving this invocation's scope and inherited producer inputs.\n")
		} else {
			text.WriteString("Retry through the CI provider, preserving this invocation's scope and inherited producer inputs.\n")
		}
		text.WriteString("If partial-retry metadata is unavailable or a producer changes, rerun that producer and its dependent gates, or the whole invocation.\n")
		text.WriteString("Do not invoke a CI task directly to bypass native success gates.\n")
		text.WriteString("Use a fresh job workspace or preserve existing output/event/export destinations elsewhere before retrying.\n")
	} else {
		target := "all originally selected tasks"
		if len(targets) != 0 {
			target = strings.Join(targets, ", ")
		}
		fmt.Fprintf(&text, "After fixing the cause, rerun the original gated targets: %s, with the same inputs and execution options.\n", target)
		text.WriteString("Fresh tasks and side effects may execute again.\n")
	}
	if !nativeCI && options.OutputDir != "" {
		fmt.Fprintf(&text, "Choose a fresh --output path instead of %q.\n", options.OutputDir)
	}
	if !nativeCI && options.EventPath != "" {
		fmt.Fprintf(&text, "Choose a fresh --events file instead of %q.\n", options.EventPath)
	}
	for _, path := range exports {
		fmt.Fprintf(&text, "Selected export destination must be absent before retry; preserve existing files elsewhere: %q.\n", path)
	}
	_, _ = io.WriteString(out, text.String())
	if os.Getenv("GITHUB_ACTIONS") == "true" && os.Getenv("GITHUB_STEP_SUMMARY") != "" {
		file, err := os.OpenFile(os.Getenv("GITHUB_STEP_SUMMARY"), os.O_WRONLY|os.O_APPEND, 0)
		if err == nil {
			_, err = fmt.Fprintf(file, "### Stoke failed\n\n<pre>%s</pre>\n", html.EscapeString(text.String()))
			if closeErr := file.Close(); err == nil {
				err = closeErr
			}
		}
		if err != nil {
			_, _ = fmt.Fprintf(out, "stoke: could not append GitHub summary: %v\n", err)
		}
	}
}
