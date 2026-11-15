package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
	"sync"

	cdevents "github.com/cdevents/sdk-go/pkg/api"
	cdeventsv05 "github.com/cdevents/sdk-go/pkg/api/v05"
	"github.com/google/uuid"
	"github.com/opencontainers/go-digest"
	packageurl "github.com/package-url/packageurl-go"
	coopr "github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/workflow"
	"oras.land/oras-go/v2/registry"
)

// CDEvents writes validated SDK events as JSONL. It observes execution without
// retaining task lifecycle state or changing scheduling and result reporting.
type CDEvents struct {
	RunID string

	writer io.Writer
	ci     *cdeventsCI
	mu     sync.Mutex
	err    error
}

type cdeventsCI struct {
	ScopeHash string `json:"scopeHash"`
	Unit      string `json:"unit"`
	Attempt   int    `json:"attempt"`
}

func NewCDEvents(writer io.Writer) (*CDEvents, error) {
	if writer == nil {
		return nil, errors.New("CDEvents writer is required")
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	return &CDEvents{RunID: id.URN(), writer: writer}, nil
}

// ConfigureCI is called before observation. Each observer keeps its fresh RunID;
// a CI unit is an isolated execution, not an observation of the host pipeline.
func (o *CDEvents) ConfigureCI(scopeHash, unit string, attempt int) error {
	if o == nil {
		return nil
	}
	if _, err := hex.DecodeString(scopeHash); err != nil || len(scopeHash) != sha256.Size*2 || unit == "" || attempt < 1 {
		return errors.New("CDEvents CI scope hash, unit and positive attempt are required")
	}
	o.ci = &cdeventsCI{ScopeHash: scopeHash, Unit: unit, Attempt: attempt}
	return nil
}

func (o *CDEvents) PipelineStarted(name string) {
	if o == nil || o.ci != nil {
		return
	}
	event, err := cdeventsv05.NewPipelineRunStartedEvent()
	if err == nil {
		event.SetSubjectPipelineName(name)
		event.SetSubjectUri(o.RunID)
	}
	o.emit(event, err, o.RunID, nil)
}

func (o *CDEvents) PipelineFinished(report *Report, runErr error) {
	if o == nil || o.ci != nil {
		return
	}
	event, err := cdeventsv05.NewPipelineRunFinishedEvent()
	facts := map[string]any{}
	if err == nil {
		outcome := "success"
		switch {
		case errors.Is(runErr, coopr.ErrCleanupFailed):
			outcome = "error"
		case errors.Is(runErr, context.Canceled), errors.Is(runErr, context.DeadlineExceeded):
			outcome = "cancel"
		case runErr != nil || report != nil && report.Status == "failed":
			outcome = "failure"
		}
		event.SetSubjectOutcome(outcome)
		if report != nil {
			var skipped []string
			for _, task := range report.Tasks {
				if task.Status == "skipped" {
					skipped = append(skipped, task.Name)
				}
			}
			if len(skipped) != 0 {
				facts["skipped"] = skipped
			}
		}
	}
	o.emit(event, err, o.RunID, facts)
}

func (o *CDEvents) TaskStarted(job workflow.Job) {
	if o == nil {
		return
	}
	event, err := cdeventsv05.NewTaskRunStartedEvent()
	if err == nil {
		event.SetSubjectTaskName(job.Task)
		if o.ci == nil {
			event.SetSubjectPipelineRun(&cdevents.Reference{Id: o.RunID})
		}
	}
	o.emit(event, err, o.subjectID("task", job.Name), cdeventsTaskFacts(TaskResult{Task: job.Task, Matrix: job.Matrix}))
}

func (o *CDEvents) TaskFinished(item TaskResult, taskErr error) {
	if o == nil || item.Status == "skipped" {
		return
	}
	outcome := map[string]string{"passed": "success", "failed": "failure", "canceled": "cancel"}[item.Status]
	if errors.Is(taskErr, coopr.ErrCleanupFailed) {
		outcome = "error"
	}
	if outcome == "" {
		o.fail(fmt.Errorf("unsupported task result status %q for CDEvents", item.Status))
		return
	}
	event, err := cdeventsv05.NewTaskRunFinishedEvent()
	if err == nil {
		event.SetSubjectTaskName(item.Task)
		event.SetSubjectOutcome(outcome)
		if o.ci == nil {
			event.SetSubjectPipelineRun(&cdevents.Reference{Id: o.RunID})
		}
	}
	o.emit(event, err, o.subjectID("task", item.Name), cdeventsTaskFacts(item))
	// Publication is observed per returned destination, including a successful
	// destination when another destination made the overall task fail.
	for _, destination := range item.Destinations {
		if destination.Status != "passed" {
			continue
		}
		artifact := cdeventsPublishedArtifact(destination.Reference)
		if artifact == "" {
			continue
		}
		published, err := cdeventsv05.NewArtifactPublishedEvent()
		o.emit(published, err, artifact, cdeventsTaskFacts(TaskResult{Task: item.Task, Matrix: item.Matrix}))
	}
}

func (o *CDEvents) BuildStarted(job workflow.Job) {
	if o == nil {
		return
	}
	event, err := cdeventsv05.NewBuildStartedEvent()
	o.emit(event, err, o.subjectID("build", job.Name), cdeventsTaskFacts(TaskResult{Task: job.Task, Matrix: job.Matrix}))
}

func (o *CDEvents) BuildFinished(job workflow.Job, result coopr.Result, runErr error) {
	if o == nil || runErr != nil {
		return
	}
	artifact := cdeventsContentArtifact(result.ManifestDigest)
	if artifact == "" {
		return
	}
	// The released SDK validates even an omitted artifactId as a PURL. Without
	// an exact artifact digest, taskRun.finished alone records the completion.
	event, err := cdeventsv05.NewBuildFinishedEvent()
	if err == nil {
		event.SetSubjectArtifactId(artifact)
	}
	o.emit(event, err, o.subjectID("build", job.Name), cdeventsTaskFacts(TaskResult{Task: job.Task, Matrix: job.Matrix}))
}

func (o *CDEvents) subjectID(kind, name string) string {
	return "urn:stoke:" + kind + ":" + strings.TrimPrefix(o.RunID, "urn:uuid:") + ":" + url.PathEscape(name)
}

// Retain selected export receipts without raw errors, engine metadata, command
// arguments, environment values or file contents.
func cdeventsTaskFacts(item TaskResult) map[string]any {
	facts := map[string]any{}
	if item.Task != "" {
		facts["task"] = item.Task
	}
	if len(item.Matrix) != 0 {
		facts["matrix"] = item.Matrix
	}
	if item.Kind != "" {
		facts["kind"] = item.Kind
	}
	if item.Cache != nil {
		facts["cache"] = item.Cache
	}
	if item.ProgressDropped != 0 {
		facts["progressDropped"] = item.ProgressDropped
	}
	if len(item.Exports) != 0 {
		exports := make([]map[string]string, 0, len(item.Exports))
		for _, receipt := range item.Exports {
			exports = append(exports, map[string]string{"state": receipt.State, "source": receipt.Source, "destination": receipt.Destination, "status": receipt.Status})
		}
		facts["exports"] = exports
	}
	return facts
}

func cdeventsPublishedArtifact(reference string) string {
	separator := strings.LastIndex(reference, "@")
	if separator <= 0 {
		return ""
	}
	content := cdeventsContentArtifact(reference[separator+1:])
	if content == "" {
		return ""
	}
	ref, err := registry.ParseReference(strings.TrimPrefix(reference, "docker://"))
	// A local transport prefix must not masquerade as an empty-port registry.
	if err != nil || strings.HasSuffix(ref.Registry, ":") {
		return content
	}
	qualifiers := packageurl.Qualifiers{{Key: "repository_url", Value: ref.Registry + "/" + ref.Repository}}
	return packageurl.NewPackageURL(packageurl.TypeOCI, "", path.Base(ref.Repository), ref.Reference, qualifiers, "").ToString()
}

func cdeventsContentArtifact(value string) string {
	d, err := digest.Parse(value)
	if err != nil || d.Algorithm() != digest.SHA256 {
		return ""
	}
	return packageurl.NewPackageURL(packageurl.TypeGeneric, "sha256", d.Encoded(), "", nil, "").ToString()
}

func (o *CDEvents) emit(event cdevents.CDEventV04, err error, subject string, facts map[string]any) {
	if err != nil {
		o.fail(err)
		return
	}
	event.SetSource("urn:stoke:observer")
	event.SetSubjectId(subject)
	event.SetChainId(o.RunID)
	if o.ci != nil {
		if facts == nil {
			facts = map[string]any{}
		}
		facts["ci"] = o.ci
	}
	if len(facts) != 0 {
		if err := event.SetCustomData("application/json", facts); err != nil {
			o.fail(err)
			return
		}
	}
	if err := cdevents.Validate(event); err != nil {
		o.fail(err)
		return
	}
	data, err := cdevents.AsJsonBytes(event)
	if err != nil {
		o.fail(err)
		return
	}
	data = append(data, '\n')
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err == nil {
		written, err := o.writer.Write(data)
		if err == nil && written != len(data) {
			err = io.ErrShortWrite
		}
		o.err = err
	}
}

func (o *CDEvents) fail(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err == nil {
		o.err = err
	}
}

func (o *CDEvents) Err() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

func cdeventsResult(report *Report, runErr error, observer *CDEvents) error {
	if err := observer.Err(); err != nil && !errors.Is(runErr, err) {
		runErr = errors.Join(runErr, fmt.Errorf("write CDEvents: %w", err))
		if report != nil {
			return persistReport(report, runErr)
		}
	}
	return runErr
}

// Finish belongs to the invocation owner, after its input and engine cleanup.
// Reconcile terminal stream failures through the existing run report as well.
func (o *CDEvents) Finish(report *Report, runErr error) error {
	o.PipelineFinished(report, runErr)
	return cdeventsResult(report, runErr, o)
}
