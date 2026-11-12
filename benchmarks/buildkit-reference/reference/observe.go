package main

import (
	"context"
	"io"
	"sync/atomic"

	"github.com/moby/buildkit/executor"
	"github.com/moby/buildkit/executor/resources/types"
)

// Count actual upstream executor calls without changing cache decisions.
type observedExecutor struct {
	executor.Executor
	runs atomic.Int64
}

func (o *observedExecutor) Run(ctx context.Context, id string, root executor.Mount, mounts []executor.Mount, process executor.ProcessInfo, started chan<- struct{}) (types.Recorder, error) {
	o.runs.Add(1)
	if process.Stdout != nil {
		process.Stdout = &runOutput{WriteCloser: process.Stdout}
	}
	if process.Stderr != nil {
		process.Stderr = &runOutput{WriteCloser: process.Stderr, stderr: true}
	}
	return o.Executor.Run(ctx, id, root, mounts, process, started)
}

type runOutput struct {
	io.WriteCloser
	stderr bool
}

func (w *runOutput) Write(p []byte) (int, error) {
	n, err := w.WriteCloser.Write(p)
	if n != 0 {
		emit(map[string]any{"phase": "run-output", "stderr": w.stderr, "data": string(p[:n])})
	}
	return n, err
}
