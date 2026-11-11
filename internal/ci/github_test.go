package ci

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func renderFixture(t *testing.T, p *Plan, planPath string) ([]byte, githubWorkflow) {
	t.Helper()
	data, err := RenderGitHub(p, planPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyGenerated(data); err != nil {
		t.Fatal(err)
	}
	var workflow githubWorkflow
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	return data, workflow
}

func renderedJobs(t *testing.T, workflow githubWorkflow) map[string]githubJob {
	t.Helper()
	jobs := map[string]githubJob{}
	if err := workflow.Jobs.Decode(&jobs); err != nil {
		t.Fatal(err)
	}
	return jobs
}

func TestGitHubDoesNotInterpretLiteralMatrixValues(t *testing.T) {
	literal := "${{ secrets.TOKEN }}; $(touch /never)"
	w := decodeWorkflow(t, `version: 1
tasks:
  tests:
    image: scratch
    strategy: {matrix: {value: ['`+literal+`']}}
`)
	p := buildPlanFixture(t, w, Config{})
	data, _ := renderFixture(t, p, ".stoke/ci-plan.json")
	if bytes.Contains(data, []byte(literal)) || p.Groups[0].Jobs[0].Matrix["value"] != literal {
		t.Fatal("provider interpreted or discarded a literal matrix value")
	}
}

func TestGitHubMatrixNamesShowSafeAuthoredValues(t *testing.T) {
	w := decodeWorkflow(t, `version: 1
tasks:
  checks:
    image: scratch
    strategy: {matrix: {check: [test, vet]}}
`)
	p := buildPlanFixture(t, w, Config{})
	_, rendered := renderFixture(t, p, ".stoke/ci-plan.json")
	job := renderedJobs(t, rendered)[p.Groups[0].ID]
	if job.Name != "checks / ${{ matrix.stoke_label }}" || job.Strategy == nil {
		t.Fatal("visible matrix labels were not selected")
	}
	for i, row := range job.Strategy.Matrix["include"] {
		selection := p.Groups[0].Jobs[i]
		if row["stoke_label"] != "check="+selection.Matrix["check"] || row["stoke_job"] != selection.ID || job.Outputs[selection.OutputKey] == "" {
			t.Fatal("human labels changed matrix identity or outputs")
		}
	}
	if label := matrixLabel(Job{ID: "fallback", Matrix: map[string]string{"z": "last", "a": "first"}}); label != "a=first, z=last" {
		t.Fatalf("matrix label is not deterministic: %q", label)
	}
	for _, value := range []string{"${{ secrets.TOKEN }}", "line\nbreak", "\x01", strings.Repeat("x", 257), string([]byte{0xff})} {
		selection := Job{ID: "fallback", Matrix: map[string]string{"axis": value}}
		if matrixLabel(selection) != selection.ID || selection.Matrix["axis"] != value {
			t.Fatal("unsafe display label was interpreted or changed runtime input")
		}
	}
}

func TestGeneratedOwnershipChecksumDetectsEdits(t *testing.T) {
	data, _ := renderFixture(t, planFixture(t), ".stoke/ci-plan.json")
	for _, modified := range [][]byte{[]byte("name: Handwritten\non: push\n"), append(bytes.Clone(data), []byte("# manual edit\n")...), bytes.Replace(data, []byte("Stoke result"), []byte("Edited result"), 1)} {
		if err := VerifyGenerated(modified); err == nil {
			t.Fatal("handwritten or edited workflow accepted")
		}
	}
	if _, err := RenderGitHub(planFixture(t), "${{ secrets.PLAN }}"); err == nil {
		t.Fatal("provider expression in plan path accepted")
	}
}

func TestRegistryCacheDirectionIsNeverImplicit(t *testing.T) {
	w := decodeWorkflow(t, "version: 1\ntasks: {build: {image: scratch}}\n")
	for _, config := range []Config{{}, {CacheFrom: []string{"cache.example.test/read"}}, {CacheTo: []string{"cache.example.test/write"}}} {
		p := buildPlanFixture(t, w, config)
		_, workflow := renderFixture(t, p, ".stoke/ci-plan.json")
		run := renderedJobs(t, workflow)[p.Groups[0].ID].Steps[1].Run
		if strings.Contains(run, "--cache-from") != (len(config.CacheFrom) > 0) || strings.Contains(run, "--cache-to") != (len(config.CacheTo) > 0) {
			t.Fatalf("cache direction inferred: %s", run)
		}
	}
}

func TestGitHubVisibleJobsNativeMatrixOutputsAndAggregate(t *testing.T) {
	p := planFixture(t)
	data, w := renderFixture(t, p, ".stoke/ci-plan.json")
	jobs := renderedJobs(t, w)
	if len(jobs) != len(p.Groups)+2 || len(w.On) != 1 || len(w.Permissions) != 0 {
		t.Fatal("setup/aggregate or caller boundary lost")
	}
	setup := jobs["stoke_context"]
	if bytes.Contains(data, []byte("env: {}")) {
		t.Fatal("empty env sections are invalid GitHub Actions syntax")
	}
	if len(setup.Needs) != 0 || setup.Outputs["scope"] != "${{ steps.scope.outputs.scope }}" || !strings.Contains(setup.Steps[0].Run, "--plan-id '"+p.ID+"'") {
		t.Fatal("isolated scope setup not bound to plan")
	}
	for _, group := range p.Groups {
		j := jobs[group.ID]
		if !slices.Equal(j.Needs, append([]string{"stoke_context"}, group.Needs...)) || j.If != "" || !slices.Equal(j.RunsOn, p.Config.Runners) {
			t.Fatal("native gates/provisioning changed")
		}
		for _, row := range group.Jobs {
			if j.Outputs[row.OutputKey] != "${{ steps.stoke.outputs."+row.OutputKey+" }}" {
				t.Fatal("missing unique variant output")
			}
		}
		for _, parentID := range group.Needs {
			for _, parent := range p.Groups {
				if parent.ID == parentID {
					for _, row := range parent.Jobs {
						if j.Env[InputEnv(row.OutputKey)] != "${{ needs."+parentID+".outputs."+row.OutputKey+" }}" {
							t.Fatal("parent metadata not passed per slot")
						}
					}
				}
			}
		}
		if group.Matrix && (j.Strategy == nil || j.Strategy.FailFast != group.FailFast || j.Strategy.MaxParallel != group.MaxParallel || len(j.Strategy.Matrix["include"]) != len(group.Jobs)) {
			t.Fatal("native matrix lost")
		}
		if _, exists := j.Env["STOKE_CI_NEEDS"]; exists {
			t.Fatal("single oversized needs JSON env restored")
		}
		if !strings.Contains(j.Steps[1].Run, "--needs-env") || j.Steps[1].ID != "stoke" {
			t.Fatal("metadata command/output step missing")
		}
	}
	result := jobs["stoke_result"]
	if len(result.Needs) != len(p.Groups)+1 || result.If != "${{ !cancelled() }}" || result.Env["STOKE_CI_RESULTS"] != "${{ toJSON(needs.*.result) }}" {
		t.Fatal("aggregate does not use compact native statuses")
	}
	for _, forbidden := range []string{"upload-artifact", "download-artifact", "--receipt", "--handoffs", "--bundle", "always()", "continue-on-error"} {
		if bytes.Contains(data, []byte(forbidden)) {
			t.Fatalf("forbidden transport/gating %q", forbidden)
		}
	}
	again, err := RenderGitHub(p, ".stoke/ci-plan.json")
	if err != nil || !bytes.Equal(data, again) {
		t.Fatal("nondeterministic renderer")
	}
}

func TestResultsRepositoryUsesExplicitCallInputOrLiteral(t *testing.T) {
	w := decodeWorkflow(t, "version: 1\ntasks: {build: {image: scratch}, test: {from: build}}\n")
	for _, repository := range []string{"", "results.example.test/team/run-results"} {
		p := buildPlanFixture(t, w, Config{ResultsRepository: repository})
		_, rendered := renderFixture(t, p, ".stoke/ci-plan.json")
		call := rendered.On["workflow_call"].(map[string]any)
		jobs := renderedJobs(t, rendered)
		if repository == "" {
			inputs, ok := call["inputs"].(map[string]any)
			if !ok || inputs["results_repository"].(map[string]any)["required"] != true {
				t.Fatal("missing required call-time result repository")
			}
		} else if len(call) != 0 {
			t.Fatal("configured repository still needs call input")
		}
		for _, g := range p.Groups {
			job := jobs[g.ID]
			want := repository
			if want == "" {
				want = "${{ inputs.results_repository }}"
			}
			if job.Env["STOKE_CI_RESULTS_REPOSITORY"] != want || !strings.Contains(job.Steps[1].Run, `--results-repository "$STOKE_CI_RESULTS_REPOSITORY"`) {
				t.Fatal("repository not wired through every visible job")
			}
		}
	}
}

func TestExplicitCoLocationKeepsMatrixInternal(t *testing.T) {
	w := decodeWorkflow(t, `version: 1
tasks:
  producer: {image: scratch}
  checks:
    from: producer
    strategy: {max-parallel: 2, fail-fast: false, matrix: {mode: [one, two]}}
`)
	p := buildPlanFixture(t, w, Config{CoLocate: []string{"producer"}})
	_, rendered := renderFixture(t, p, ".stoke/ci-plan.json")
	g := p.Groups[0]
	j := renderedJobs(t, rendered)[g.ID]
	if len(p.Groups) != 1 || j.Strategy != nil || len(g.Jobs[0].Execute) != 3 || p.RequiresResults {
		t.Fatal("explicit local matrix split or requires unnecessary registry")
	}
	call := rendered.On["workflow_call"].(map[string]any)
	if len(call) != 0 {
		t.Fatal("no-registry explicit choice needs result input")
	}
}

func TestGeneratedArgumentsRemainLiteral(t *testing.T) {
	p := buildPlanFixture(t, decodeWorkflow(t, graphFixture), Config{Targets: []string{"package"}, CacheFrom: []string{"cache.example.test/first", "cache.example.test/second"}, CacheTo: []string{"cache.example.test/write"}})
	planPath := ".stoke/plan's $(printf unsafe).json"
	_, w := renderFixture(t, p, planPath)
	job := renderedJobs(t, w)[p.Groups[0].ID]
	dir := t.TempDir()
	capture := filepath.Join(dir, "arguments")
	if err := os.WriteFile(filepath.Join(dir, "stoke"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CAPTURE\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	scope := `{"invocation":"literal;$(true)"}`
	output := filepath.Join(dir, "output file")
	cmd := exec.Command("bash", "-c", job.Steps[1].Run)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "CAPTURE="+capture, "STOKE_CI_JOB=job with space;$(true)", "RUNNER_TEMP="+dir+"/runner space", "STOKE_CI_SCOPE="+scope, "GITHUB_RUN_ATTEMPT=3", "GITHUB_OUTPUT="+output)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("command: %v %s", err, out)
	}
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	want := []string{"ci", "run", "--plan", planPath, "--plan-id", p.ID, "--job", "job with space;$(true)", "--scope", scope, "--attempt", "3", "--needs-env", "--github-output", output, "--cache-dir", dir + "/runner space/stoke-cache", "--output", dir + "/runner space/stoke-output", "--cache-from", "cache.example.test/first", "--cache-from", "cache.example.test/second", "--cache-to", "cache.example.test/write"}
	if !slices.Equal(args, want) {
		t.Fatalf("arguments changed\ngot %#v\nwant %#v", args, want)
	}
}

func TestJobContainerIsExplicitAndCoversEveryGeneratedJob(t *testing.T) {
	for _, source := range []string{graphFixture, hostGraphFixture} {
		w := decodeWorkflow(t, source)
		base := buildPlanFixture(t, w, Config{})
		image := "registry.example.test/team/stoke@sha256:" + strings.Repeat("a", 64)
		previous := base.ID
		for _, config := range []Config{
			{},
			{GitHubTokenAuth: true},
			{JobContainerImage: image},
			{JobContainerImage: image, JobContainerOptions: "--user=1001:1001 --cpus=2"},
			{JobContainerImage: image, GitHubTokenAuth: true},
			{JobContainerImage: "ghcr.io/team/stoke@sha256:" + strings.Repeat("a", 64), GitHubTokenAuth: true},
		} {
			p := buildPlanFixture(t, w, config)
			data, rendered := renderFixture(t, p, ".stoke/ci-plan.json")
			if (config.JobContainerImage != "" || config.GitHubTokenAuth) && p.ID == previous {
				t.Fatal("job container or authentication missing from plan identity")
			}
			previous = p.ID
			for id, job := range renderedJobs(t, rendered) {
				if config.JobContainerImage == "" {
					if job.Container != nil {
						t.Fatalf("implicit container on %s", id)
					}
				} else if job.Container == nil || job.Container.Image != config.JobContainerImage || job.Container.Options != config.JobContainerOptions {
					t.Fatalf("container selection changed on %s: %#v", id, job.Container)
				}
				if job.Container != nil {
					wantCredentials := config.GitHubTokenAuth && strings.HasPrefix(config.JobContainerImage, "ghcr.io/")
					if (len(job.Container.Credentials) != 0) != wantCredentials {
						t.Fatalf("container pull authentication crossed registry/opt-in boundary on %s", id)
					}
				}
				login, cleanup, execution := 0, 0, false
				for _, step := range job.Steps {
					if step.ID == "stoke" {
						execution = true
						if strings.Contains(step.Run, "--authfile") != config.GitHubTokenAuth {
							t.Fatalf("execution authentication mismatch on %s", id)
						}
					}
					if step.Name == "Authenticate GHCR" {
						login++
						if step.Env["STOKE_GHCR_TOKEN"] != "${{ secrets.GITHUB_TOKEN }}" || !strings.Contains(step.Run, "--password-stdin ghcr.io") || !strings.Contains(step.Run, "mktemp -d") {
							t.Fatalf("authentication lost private path or native stdin login on %s", id)
						}
					} else if step.Env["STOKE_GHCR_TOKEN"] != "" {
						t.Fatalf("credential environment escaped login step on %s", id)
					}
					if step.Name == "Remove registry credentials" && step.If == "${{ always() && steps.stoke_auth.outputs.directory != '' }}" && step.Env["STOKE_GHCR_AUTH_DIR"] == "${{ steps.stoke_auth.outputs.directory }}" {
						cleanup++
					}
				}
				wantLogin := 0
				if execution && config.GitHubTokenAuth {
					wantLogin = 1
				}
				if login != wantLogin || cleanup != wantLogin || len(rendered.Permissions) != 0 {
					t.Fatalf("authentication lifecycle or caller-owned permissions changed on %s", id)
				}
			}
			if bytes.Contains(data, []byte("--privileged")) || bytes.Contains(data, []byte("security-opt")) {
				t.Fatal("renderer silently relaxes container security")
			}
			encoded, err := Encode(p)
			if err != nil {
				t.Fatal(err)
			}
			round, err := Decode(bytes.NewReader(encoded))
			if err != nil || round.ID != p.ID || round.Config.JobContainerImage != config.JobContainerImage || round.Config.JobContainerOptions != config.JobContainerOptions || round.Config.GitHubTokenAuth != config.GitHubTokenAuth {
				t.Fatal("container configuration did not roundtrip", err)
			}
		}
	}
}

func TestJobContainerRejectsMutableReferencesAndExpressions(t *testing.T) {
	image := "registry.example.test/team/stoke@sha256:" + strings.Repeat("a", 64)
	for _, config := range []Config{
		{JobContainerImage: "registry.example.test/team/stoke:latest"},
		{JobContainerImage: "registry.example.test/team/stoke:bad tag@sha256:" + strings.Repeat("a", 64)},
		{JobContainerImage: "registry.example.test/team/stoke@sha256:invalid"},
		{JobContainerImage: "${{ inputs.image }}"},
		{JobContainerOptions: "--cpus=2"},
		{JobContainerImage: image, JobContainerOptions: "${{ secrets.OPTIONS }}"},
		{JobContainerImage: image, JobContainerOptions: "--cpus=2\n--privileged"},
		{JobContainerImage: image, JobContainerOptions: strings.Repeat("x", 4097)},
	} {
		if _, err := Build(decodeWorkflow(t, graphFixture), config); err == nil {
			t.Fatalf("accepted invalid container configuration: %#v", config)
		}
	}
}
