package runner

import (
	"fmt"

	coopr "github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/workflow"
)

// A stored image keeps its producer's actual platform, including explicit
// native FROM choices. Retagging it as a different architecture is not a build.
func taskPlatform(task workflow.Task, results map[string]coopr.Result) (string, error) {
	requested, err := task.ResolvedPlatform()
	if err != nil {
		return "", fmt.Errorf("platform: %w", err)
	}
	if task.From == "" {
		return requested, nil
	}
	producer, ok := results[task.From]
	if !ok || producer.ImageID == "" {
		return "", fmt.Errorf("task %q did not return a runnable image", task.From)
	}
	actual, err := workflow.NormalizePlatform(producer.Platform)
	if err != nil {
		return "", fmt.Errorf("task %q did not report a valid image platform: %w", task.From, err)
	}
	if requested != "" && requested != actual {
		return "", fmt.Errorf("platform %q conflicts with task %q image platform %q", requested, task.From, actual)
	}
	return actual, nil
}
