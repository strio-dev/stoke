package source

import "github.com/strio-dev/stoke/internal/workflow"

func taskMountLists(task workflow.Task) [][]workflow.Mount {
	mounts := make([][]workflow.Mount, 0, len(task.Steps)+1)
	mounts = append(mounts, task.Mounts)
	for _, step := range task.Steps {
		mounts = append(mounts, step.Mounts)
	}
	return mounts
}
