package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	cdevents "github.com/cdevents/sdk-go/pkg/api"
	cdeventsv05 "github.com/cdevents/sdk-go/pkg/api/v05"
	packageurl "github.com/package-url/packageurl-go"
	cooprwire "github.com/strio-dev/coopr/api"
	coopr "github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/workflow"
)

func readCDEvents(t *testing.T, output *bytes.Buffer) []cdevents.CDEventV04 {
	t.Helper()
	var events []cdevents.CDEventV04
	for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		event, err := cdeventsv05.NewFromJsonBytes(line)
		if err != nil {
			t.Fatal(err)
		}
		if err := cdevents.Validate(event); err != nil {
			t.Fatalf("invalid %s: %v", event.GetType(), err)
		}
		events = append(events, event)
	}
	return events
}

func cdeventFacts(t *testing.T, event cdevents.CDEventV04) map[string]any {
	t.Helper()
	var facts map[string]any
	if err := event.GetCustomDataAs(&facts); err != nil {
		t.Fatal(err)
	}
	return facts
}

func TestCDEventsLifecycleAndExistingOutputFacts(t *testing.T) {
	var output bytes.Buffer
	observer, err := NewCDEvents(&output)
	if err != nil {
		t.Fatal(err)
	}
	job := workflow.Job{Name: "test[os=linux/arm64]", Task: "test", Matrix: map[string]string{"os": "linux/arm64"}}
	digest := "sha256:" + strings.Repeat("a", 64)
	observer.PipelineStarted("workflow.yaml")
	observer.TaskStarted(job)
	observer.BuildStarted(job)
	observer.BuildFinished(job, coopr.Result{ManifestDigest: digest}, nil)
	observer.TaskFinished(TaskResult{
		Name: job.Name, Task: job.Task, Matrix: job.Matrix, Status: "passed", Kind: "image",
		Cache: &coopr.CacheStats{Hits: 1}, ProgressDropped: 3,
		Error: "private-error", Metadata: map[string]json.RawMessage{"private": json.RawMessage(`"private-metadata"`)},
		Exports: []cooprwire.ExportResult{{State: "prefix", Source: "/report", Destination: "reports/report", Status: "passed", Error: "private-export-error"}},
	}, nil)
	observer.PipelineFinished(&Report{Status: "passed", Tasks: []TaskResult{{Name: "not-started", Status: "skipped"}}}, nil)
	if err := observer.Err(); err != nil {
		t.Fatal(err)
	}
	events := readCDEvents(t, &output)
	wantTypes := []string{"pipelinerun.started", "taskrun.started", "build.started", "build.finished", "taskrun.finished", "pipelinerun.finished"}
	if len(events) != len(wantTypes) {
		t.Fatalf("events = %d: %s", len(events), output.String())
	}
	ids := map[string]bool{}
	for i, event := range events {
		if event.GetType().String() != "dev.cdevents."+wantTypes[i]+".0.3.0" || event.GetVersion() != "0.5.1" || event.GetSource() != "urn:stoke:observer" || event.GetChainId() != observer.RunID || event.GetTimestamp().IsZero() || ids[event.GetId()] {
			t.Fatalf("unexpected event %d: %+v", i, event)
		}
		ids[event.GetId()] = true
	}
	if events[0].GetSubjectId() != observer.RunID || events[5].GetSubjectId() != observer.RunID || events[1].GetSubjectId() != events[4].GetSubjectId() || !strings.HasSuffix(events[1].GetSubjectId(), url.PathEscape(job.Name)) || events[2].GetSubjectId() != events[3].GetSubjectId() || events[1].GetSubjectId() == events[2].GetSubjectId() {
		t.Fatal("pipeline/task/build subject identity was not preserved")
	}
	if !strings.HasPrefix(events[1].GetSubjectId(), "urn:stoke:task:") || !strings.HasPrefix(events[2].GetSubjectId(), "urn:stoke:build:") {
		t.Fatal("task/build subjects are not in the Stoke URN namespace")
	}
	finished := events[4].(*cdeventsv05.TaskRunFinishedEvent)
	if finished.Subject.Content.Outcome != "success" || finished.Subject.Content.PipelineRun.Id != observer.RunID {
		t.Fatalf("task completion = %+v", finished.Subject.Content)
	}
	facts := cdeventFacts(t, finished)
	if facts["matrix"].(map[string]any)["os"] != job.Matrix["os"] || facts["cache"].(map[string]any)["Hits"] != float64(1) || facts["progressDropped"] != float64(3) || len(facts["exports"].([]any)) != 1 {
		t.Fatalf("task facts = %+v", facts)
	}
	if got := cdeventFacts(t, events[5])["skipped"].([]any); len(got) != 1 || got[0] != "not-started" {
		t.Fatalf("skipped summary = %v", got)
	}
	if strings.Contains(output.String(), "private-") || strings.Contains(output.String(), "testcaserun") {
		t.Fatal("private error/metadata or inferred test semantics escaped")
	}
}

func TestCDEventsCIUsesActualAttemptAndFreshInvocation(t *testing.T) {
	var previous string
	for range 2 {
		var output bytes.Buffer
		observer, err := NewCDEvents(&output)
		if err != nil {
			t.Fatal(err)
		}
		if err := observer.ConfigureCI(strings.Repeat("b", 64), "unit-check", 2); err != nil {
			t.Fatal(err)
		}
		observer.PipelineStarted("remote-workflow")
		observer.TaskStarted(workflow.Job{Name: "check", Task: "check"})
		observer.TaskFinished(TaskResult{Name: "check", Task: "check", Status: "passed"}, nil)
		observer.PipelineFinished(&Report{Status: "passed"}, nil)
		if err := observer.Err(); err != nil {
			t.Fatal(err)
		}
		events := readCDEvents(t, &output)
		if len(events) != 2 || events[0].GetSubjectId() != events[1].GetSubjectId() || !strings.HasPrefix(events[0].GetSubjectId(), "urn:stoke:task:") || observer.RunID == previous || !strings.HasPrefix(observer.RunID, "urn:uuid:") {
			t.Fatalf("CI invocation events = %+v", events)
		}
		previous = observer.RunID
		for _, event := range events {
			facts := cdeventFacts(t, event)["ci"].(map[string]any)
			if facts["scopeHash"] != strings.Repeat("b", 64) || facts["unit"] != "unit-check" || facts["attempt"] != float64(2) {
				t.Fatalf("CI facts = %+v", facts)
			}
		}
		if events[0].(*cdeventsv05.TaskRunStartedEvent).Subject.Content.PipelineRun != nil {
			t.Fatal("isolated CI task claimed a pipeline run")
		}
	}
}

func TestCDEventsExactPartialPublication(t *testing.T) {
	var output bytes.Buffer
	observer, err := NewCDEvents(&output)
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("c", 64)
	reference := "registry.example/team/image@" + digest
	observer.TaskFinished(TaskResult{Name: "publish", Status: "failed", Kind: "publish", Digest: digest, Reference: reference, Destinations: []cooprwire.Destination{
		{Status: "passed", Reference: reference},
		{Status: "passed", Reference: "docker://registry.example:5000/team/other:chosen@" + digest},
		{Status: "failed", Reference: reference},
		{Status: "succeeded", Reference: "registry.example/team/wrong@" + digest}, // Not a Coopr result status.
		{Status: "passed", Reference: "registry.example/team/image:latest"},
		{Status: "passed", Reference: "registry.example/team/image@sha256:bad"},
		{Status: "passed", Reference: "oci:/tmp/layout@" + digest},
		{Status: "passed", Reference: "oci-archive:/tmp/image.tar@" + digest},
		{Status: "passed", Reference: "/tmp/with@name/layout@" + digest},
		{Status: "passed", Reference: "unknown-native-format"},
		{Status: "passed", Reference: "@" + digest},
		{Status: "passed", Destination: reference},
	}}, errors.New("another destination failed"))
	if err := observer.Err(); err != nil {
		t.Fatal(err)
	}
	events := readCDEvents(t, &output)
	if len(events) != 6 || events[0].(*cdeventsv05.TaskRunFinishedEvent).Subject.Content.Outcome != "failure" || events[1].GetType().String() != "dev.cdevents.artifact.published.0.3.0" || events[2].GetType().String() != "dev.cdevents.artifact.published.0.3.0" {
		t.Fatalf("partial publication events = %+v", events)
	}
	purl, err := packageurl.FromString(events[1].GetSubjectId())
	if err != nil || purl.Type != packageurl.TypeOCI || purl.Name != "image" || purl.Version != digest || purl.Qualifiers.Map()["repository_url"] != "registry.example/team/image" {
		t.Fatalf("published artifact = %+v, %v", purl, err)
	}
	purl, err = packageurl.FromString(events[2].GetSubjectId())
	if err != nil || purl.Type != packageurl.TypeOCI || purl.Name != "other" || purl.Version != digest || purl.Qualifiers.Map()["repository_url"] != "registry.example:5000/team/other" {
		t.Fatalf("native transport publication = %+v, %v", purl, err)
	}
	for _, event := range events[3:] {
		purl, err := packageurl.FromString(event.GetSubjectId())
		if err != nil || event.GetType().String() != "dev.cdevents.artifact.published.0.3.0" || purl.Type != packageurl.TypeGeneric || purl.Namespace != "sha256" || purl.Name != strings.Repeat("c", 64) || purl.Version != "" || len(purl.Qualifiers) != 0 {
			t.Fatalf("native/local content identity = %+v, %v", purl, err)
		}
	}
}

func TestCDEventsBuildArtifactIdentityIsContentAddressed(t *testing.T) {
	var output bytes.Buffer
	observer, err := NewCDEvents(&output)
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("d", 64)
	for _, name := range []string{"UPPERCASE", "namespace/build", "test[os=linux/arm64]"} {
		observer.BuildFinished(workflow.Job{Name: name}, coopr.Result{ManifestDigest: digest}, nil)
	}
	if err := observer.Err(); err != nil {
		t.Fatal(err)
	}
	events := readCDEvents(t, &output)
	if len(events) != 3 {
		t.Fatalf("build completions = %d", len(events))
	}
	for _, event := range events {
		artifact := event.(*cdeventsv05.BuildFinishedEvent).Subject.Content.ArtifactId
		purl, err := packageurl.FromString(artifact)
		if err != nil || purl.Type != packageurl.TypeGeneric || purl.Namespace != "sha256" || purl.Name != strings.Repeat("d", 64) || purl.Version != "" || artifact != events[0].(*cdeventsv05.BuildFinishedEvent).Subject.Content.ArtifactId {
			t.Fatalf("build artifact identity = %q (%+v), %v", artifact, purl, err)
		}
	}
}

func TestCDEventsFailureCancelAndUnstartedTasks(t *testing.T) {
	var output bytes.Buffer
	observer, err := NewCDEvents(&output)
	if err != nil {
		t.Fatal(err)
	}
	for _, completion := range []struct {
		status string
		err    error
	}{
		{"failed", errors.New("command failed")},
		{"canceled", context.Canceled},
		{"failed", errors.Join(context.Canceled, coopr.ErrCleanupFailed)},
		{"skipped", nil},
	} {
		observer.TaskFinished(TaskResult{Name: completion.status, Status: completion.status, Exports: []cooprwire.ExportResult{{Source: "/report", Destination: "report", Status: "passed"}}}, completion.err)
	}
	observer.BuildFinished(workflow.Job{Name: "failed-build"}, coopr.Result{}, errors.New("command failed"))
	observer.BuildFinished(workflow.Job{Name: "no-artifact"}, coopr.Result{}, nil)
	observer.BuildFinished(workflow.Job{Name: "invalid-artifact"}, coopr.Result{ManifestDigest: "sha256:bad"}, nil)
	observer.PipelineFinished(nil, context.Canceled)
	observer.PipelineFinished(nil, errors.Join(context.Canceled, coopr.ErrCleanupFailed))
	if err := observer.Err(); err != nil {
		t.Fatal(err)
	}
	events := readCDEvents(t, &output)
	if len(events) != 5 || events[0].(*cdeventsv05.TaskRunFinishedEvent).Subject.Content.Outcome != "failure" || events[1].(*cdeventsv05.TaskRunFinishedEvent).Subject.Content.Outcome != "cancel" || events[2].(*cdeventsv05.TaskRunFinishedEvent).Subject.Content.Outcome != "error" || events[3].(*cdeventsv05.PipelineRunFinishedEvent).Subject.Content.Outcome != "cancel" || events[4].(*cdeventsv05.PipelineRunFinishedEvent).Subject.Content.Outcome != "error" {
		t.Fatalf("completion events = %+v", events)
	}
	if got := cdeventFacts(t, events[0])["exports"].([]any)[0].(map[string]any)["status"]; got != "passed" {
		t.Fatalf("failed task lost completed report receipt: %v", got)
	}
}

func TestCDEventsValidationFailsBeforeWriting(t *testing.T) {
	var output bytes.Buffer
	observer, err := NewCDEvents(&output)
	if err != nil {
		t.Fatal(err)
	}
	event, err := cdeventsv05.NewTaskRunStartedEvent()
	if err != nil {
		t.Fatal(err)
	}
	observer.emit(event, nil, "", nil) // Empty subject IDs violate the SDK schema.
	first := observer.Err()
	if first == nil {
		t.Fatal("schema-invalid event was accepted")
	}
	observer.TaskStarted(workflow.Job{Name: "later"})
	if output.Len() != 0 || observer.Err() != first {
		t.Fatal("validation failure was replaced or output continued")
	}
	for _, invalid := range []struct {
		scope, unit string
		attempt     int
	}{{"bad", "unit", 1}, {strings.Repeat("a", 64), "", 1}, {strings.Repeat("a", 64), "unit", 0}} {
		if err := observer.ConfigureCI(invalid.scope, invalid.unit, invalid.attempt); err == nil {
			t.Fatalf("invalid CI identity accepted: %+v", invalid)
		}
	}
}

func TestCDEventsTerminalWriteFailureUsesExistingReport(t *testing.T) {
	var output bytes.Buffer
	observer, err := NewCDEvents(&output)
	if err != nil {
		t.Fatal(err)
	}
	observer.PipelineStarted("fixture")
	report := &Report{Status: "passed", Directory: t.TempDir()}
	if err := persistReport(report, nil); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("terminal stream failure")
	writer := &cdeventsFailWriter{err: failure}
	observer.writer = writer
	result := observer.Finish(report, nil)
	if !errors.Is(result, failure) || report.Status != "failed" {
		t.Fatal("terminal write failure did not fail the report", result)
	}
	data, err := os.ReadFile(filepath.Join(report.Directory, "run.json"))
	if err != nil || !bytes.Contains(data, []byte(`"status": "failed"`)) {
		t.Fatalf("persisted report is stale: %s (%v)", data, err)
	}
	if result = observer.Finish(report, result); writer.calls != 1 || strings.Count(result.Error(), failure.Error()) != 1 {
		t.Fatal("sticky stream error was duplicated or retried", result)
	}
}

type cdeventsFailWriter struct {
	err   error
	calls int
}

func (w *cdeventsFailWriter) Write(p []byte) (int, error) {
	w.calls++
	return 0, w.err
}

func TestCDEventsWriterFailureAndConcurrentEmission(t *testing.T) {
	failure := errors.New("event sink failed")
	for _, test := range []struct{ returned, want error }{{failure, failure}, {nil, io.ErrShortWrite}} {
		writer := &cdeventsFailWriter{err: test.returned}
		observer, err := NewCDEvents(writer)
		if err != nil {
			t.Fatal(err)
		}
		observer.TaskStarted(workflow.Job{Name: "first"})
		observer.TaskFinished(TaskResult{Name: "first", Status: "passed"}, nil)
		if !errors.Is(observer.Err(), test.want) || writer.calls != 1 {
			t.Fatalf("error = %v, writes = %d", observer.Err(), writer.calls)
		}
	}
	var output bytes.Buffer
	observer, err := NewCDEvents(&output)
	if err != nil {
		t.Fatal(err)
	}
	var pending sync.WaitGroup
	for range 8 {
		pending.Go(func() { observer.BuildStarted(workflow.Job{Name: "parallel"}) })
	}
	pending.Wait()
	if err := observer.Err(); err != nil || len(readCDEvents(t, &output)) != 8 {
		t.Fatalf("concurrent events: %v", err)
	}
	if _, err := NewCDEvents(nil); err == nil {
		t.Fatal("nil writer accepted")
	}
	var disabled *CDEvents
	disabled.PipelineStarted("")
	disabled.PipelineFinished(nil, nil)
	disabled.TaskStarted(workflow.Job{})
	disabled.TaskFinished(TaskResult{}, nil)
	disabled.BuildStarted(workflow.Job{})
	disabled.BuildFinished(workflow.Job{}, coopr.Result{}, nil)
	if disabled.Err() != nil || disabled.ConfigureCI("", "", 0) != nil {
		t.Fatal("disabled observer was not inert")
	}
}
