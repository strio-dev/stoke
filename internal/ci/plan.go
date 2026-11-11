// Package ci maps Stoke's existing execution graph to reviewable CI jobs.
package ci

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"

	cooprwire "github.com/strio-dev/coopr/api"
	"github.com/strio-dev/stoke/internal/assets"
	"github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/runner"
	"github.com/strio-dev/stoke/internal/workflow"
	"oras.land/oras-go/v2/registry"
)

const Version = 2
const MaxPlanBytes = 4 << 20

// MaxGroups reserves provider jobs for invocation setup and the aggregate result.
const MaxGroups = 254
const MaxNativeMetadataBytes = 1536
const maxNativeOutputBytes = 1 << 20
const maxWorkflowOutputBytes = 50 << 20
const maxNativeInputEnvBytes = 1 << 20

type Config struct {
	JobContainerImage   string            `json:"jobContainerImage,omitempty"`
	JobContainerOptions string            `json:"jobContainerOptions,omitempty"`
	Workflow            string            `json:"workflow"`
	Context             string            `json:"context"`
	Targets             []string          `json:"targets,omitempty"`
	Inputs              map[string]string `json:"inputs,omitempty"`
	Runners             []string          `json:"runners"`
	TimeoutMinutes      int               `json:"timeoutMinutes"`
	CacheFrom           []string          `json:"cacheFrom,omitempty"`
	CacheTo             []string          `json:"cacheTo,omitempty"`
	ResultsRepository   string            `json:"resultsRepository,omitempty"`
	CoLocate            []string          `json:"coLocate,omitempty"`
	Network             string            `json:"network,omitempty"`
}

type Engine struct {
	APIVersion    int      `json:"apiVersion"`
	Capabilities  []string `json:"capabilities"`
	ProductionPin *string  `json:"productionPin"`
	PinStatus     string   `json:"pinStatus"`
}

type Plan struct {
	Version         int                        `json:"version"`
	GeneratedBy     string                     `json:"generatedBy"`
	ID              string                     `json:"id"`
	Config          Config                     `json:"config"`
	WorkflowDigest  string                     `json:"workflowDigest"`
	Engine          Engine                     `json:"engine"`
	Groups          []Group                    `json:"groups"`
	Limitations     []string                   `json:"limitations"`
	Environments    map[string]string          `json:"environments,omitempty"`
	RequiresResults bool                       `json:"requiresResults"`
	Host            map[string]HostRequirement `json:"host,omitempty"`
}

// HostRequirement is reviewable provisioning metadata, never execution
// authority. Callers still opt in separately for each generated invocation.
type HostRequirement struct {
	Platform     string   `json:"platform"`
	Tools        []string `json:"tools"`
	Capabilities []string `json:"capabilities,omitempty"`
	Effects      string   `json:"effects"`
	Cache        bool     `json:"cache"`
}

type Group struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Members     []string `json:"members"`
	Needs       []string `json:"needs,omitempty"`
	MaxParallel int      `json:"maxParallel"`
	FailFast    bool     `json:"failFast"`
	Matrix      bool     `json:"matrix"`
	Jobs        []Job    `json:"jobs"`
}

type Job struct {
	ID        string            `json:"id"`
	Task      string            `json:"task"`
	Matrix    map[string]string `json:"matrix,omitempty"`
	Data      []string          `json:"data,omitempty"`
	Execute   []string          `json:"execute"`
	Imports   []string          `json:"imports,omitempty"`
	Exports   []string          `json:"exports,omitempty"`
	OutputKey string            `json:"outputKey"`
}

var limitations = []string{
	"GitHub owns scheduling: unrelated jobs may continue after another job fails; dependency success gates are preserved, not local workflow-wide cancellation timing.",
	"Runners must already provide Stoke, compatible Coopr API v1, Linux rootless execution and any required binfmt/QEMU; generation installs nothing.",
	"GitHub.com reusable workflows only; GitHub Enterprise Server and native ARM/restricted-runner execution are not qualified.",
	"Local workflow/context selectors must be checkout-relative; OCI workflow imports remain supported, but OCI root selectors and live host mounts are not generated.",
	"Authored stages and matrix variants use separate native CI jobs by default. Cross-job data uses exact immutable OCI results; missing results fail rather than rebuilding producers after gates.",
	"Stable cross-machine external resolution still requires pinned references. OCI result bindings freeze produced results, not unrelated mutable or undeclared inputs. Coopr retains execution and cache ownership.",
	"Co-location is explicit. The ordinary scheduler preserves internal gates and matrix policy; shared inline environments spanning jobs become visible producer jobs. Coopr cache volumes remain disposable acceleration.",
	"The generator reserves native jobs for invocation setup and aggregate gating, with at most 254 execution groups. Unique per-variant native outputs are bounded to GitHub output limits. Missing retry metadata requires rerunning producers and dependent gates.",
}

// Build uses the same resolved workflow and Expand implementation as local runs.
// Provider configuration does not alter task definitions or invent new edges.
func Build(w *workflow.Workflow, config Config) (*Plan, error) {
	config, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	expanded, err := w.Expand(config.Targets)
	if err != nil {
		return nil, err
	}
	p := &Plan{Version: Version, GeneratedBy: "stoke-ci", Config: config, Limitations: slices.Clone(limitations), Environments: map[string]string{}}
	p.Engine = Engine{APIVersion: cooprwire.Version, PinStatus: "Unpinned: supply a compatible explicitly selected executable; production distribution remains separate."}
	nodes := slices.Clone(expanded.Groups)
	logical, concrete := map[string]int{}, map[string]int{}
	definitions := map[string]workflow.Task{}
	for gi, group := range nodes {
		logical[group.Name] = gi
		for _, job := range group.Jobs {
			if len(job.Name) > 512 {
				return nil, fmt.Errorf("concrete CI producer name exceeds the 512-byte result identity limit")
			}
			concrete[job.Name], definitions[job.Name] = gi, job.Definition
		}
	}
	union := newPartition(len(nodes))
	forced := map[int]bool{}
	// Only an explicit request contracts authored data-connected stages.
	adjacent := make([][]int, len(nodes))
	for gi, group := range nodes {
		for _, job := range group.Jobs {
			for _, name := range dataSources(job.Definition) {
				predecessor, ok := concrete[name]
				if !ok {
					return nil, fmt.Errorf("unknown CI data producer %q", name)
				}
				adjacent[gi] = append(adjacent[gi], predecessor)
				adjacent[predecessor] = append(adjacent[predecessor], gi)
			}
		}
	}
	shared, err := runner.SharedEnvironmentConsumers(expanded, config.Context)
	if err != nil {
		return nil, fmt.Errorf("shared CI environments: %w", err)
	}
	for _, consumers := range shared {
		first := concrete[consumers[0]]
		for _, name := range consumers[1:] {
			other := concrete[name]
			adjacent[first] = append(adjacent[first], other)
			adjacent[other] = append(adjacent[other], first)
		}
	}
	for _, name := range config.CoLocate {
		start, ok := logical[name]
		if !ok {
			return nil, fmt.Errorf("co-locate task %q is not selected", name)
		}
		seen, pending := map[int]bool{}, []int{start}
		for len(pending) > 0 {
			node := pending[0]
			pending = pending[1:]
			if seen[node] {
				continue
			}
			seen[node] = true
			forced[node] = true
			union.join(start, node)
			pending = append(pending, adjacent[node]...)
		}
	}
	// Shared hidden producers are real data edges. Promote them instead of hiding
	// authored stages, using the same path resolution and lowering as local runs.
	invocation := func(name string) string {
		gi := concrete[name]
		root := union.find(gi)
		merged := false
		for i := range nodes {
			if i != gi && union.find(i) == root {
				merged = true
				break
			}
		}
		if merged || forced[gi] || len(nodes[gi].Jobs) == 1 {
			return fmt.Sprintf("unit:%d", root)
		}
		return "variant:" + name
	}
	for _, consumers := range shared {
		together := true
		first := invocation(consumers[0])
		for _, name := range consumers[1:] {
			if invocation(name) != first {
				together = false
			}
		}
		if together {
			continue
		}
		identity, err := digestJSON(consumers)
		if err != nil {
			return nil, err
		}
		name := "stoke-environment-" + identity[:24]
		if _, exists := logical[name]; exists {
			return nil, fmt.Errorf("synthetic CI environment collides with task %q", name)
		}
		representative := consumers[0]
		original := definitions[representative]
		producer := workflow.Task{Coopr: original.Environment, ComponentDir: original.ComponentDir, Contexts: maps.Clone(original.Contexts), Cache: original.Cache, Needs: original.Dependencies()}
		platform, err := original.ResolvedPlatform()
		if err != nil {
			return nil, err
		}
		if platform != "" {
			producer.Platform = &workflow.Platform{Value: &platform}
		}
		filesystem := false
		producer.Outputs = &workflow.Outputs{Filesystem: &filesystem}
		index := len(nodes)
		logical[name], concrete[name], definitions[name] = index, index, producer
		nodes = append(nodes, workflow.Group{Name: name, MaxParallel: 1, FailFast: true, Jobs: []workflow.Job{{Name: name, Task: name, Definition: producer}}})
		union.parent = append(union.parent, index)
		p.Environments[name] = representative
		for _, consumer := range consumers {
			task := definitions[consumer]
			task.Environment = nil
			task.From = name
			definitions[consumer] = task
		}
	}
	dependencies := make([][]int, len(nodes))
	capabilities := map[string]bool{}
	type semanticJob struct {
		Name        string        `json:"name"`
		Definition  workflow.Task `json:"definition"`
		Assets      []assetDigest `json:"assets,omitempty"`
		MaxParallel int           `json:"maxParallel"`
		FailFast    bool          `json:"failFast"`
	}
	semantics := []semanticJob{}
	assetSets := map[string][]assetDigest{}
	for gi, group := range nodes {
		for _, job := range group.Jobs {
			task := definitions[job.Name]
			if err := portableTask(task); err != nil {
				return nil, fmt.Errorf("task %q: %w", job.Name, err)
			}
			for _, name := range task.Dependencies() {
				predecessor, ok := logical[name]
				if !ok {
					predecessor, ok = concrete[name]
				}
				if !ok || predecessor == gi {
					return nil, fmt.Errorf("task %q has unresolved CI prerequisite %q", job.Name, name)
				}
				dependencies[gi] = append(dependencies[gi], predecessor)
			}
			// Bind original authored definitions; runtime paths never enter the plan hash.
			semantic := job.Definition
			var files []assetDigest
			if semantic.ComponentDir != "" {
				var ok bool
				files, ok = assetSets[semantic.ComponentDir]
				if !ok {
					files, err = componentAssets(semantic.ComponentDir)
					if err != nil {
						return nil, err
					}
					assetSets[semantic.ComponentDir] = files
				}
			}
			semantic.ComponentDir = ""
			if gi < len(expanded.Groups) {
				semantics = append(semantics, semanticJob{Name: job.Name, Definition: semantic, Assets: files, MaxParallel: group.MaxParallel, FailFast: group.FailFast})
			}
			switch {
			case task.Host != nil:
				// Host execution uses provisioned tools, not the Coopr API.
			case task.Publish != nil:
				capabilities["publish"] = true
			case task.Coopr != nil:
				if task.OutputKind() == "component" {
					capabilities["component"] = true
				} else {
					capabilities["build"] = true
				}
			default:
				capabilities["execute"] = true
			}
			if task.Environment != nil {
				capabilities["build"] = true
			}
			if len(task.AllMounts()) != 0 {
				capabilities["run-mounts"] = true
			}
			platform, err := task.ResolvedPlatform()
			if err != nil {
				return nil, err
			}
			if platform != "" && (task.Coopr != nil || task.Environment != nil) {
				capabilities["build-platform"] = true
			}
			if task.Host != nil {
				if p.Host == nil {
					p.Host = map[string]HostRequirement{}
				}
				effects := task.Host.Effects
				if effects == "" {
					effects = "external"
				}
				p.Host[job.Name] = HostRequirement{Platform: platform, Tools: slices.Clone(task.Host.Tools), Capabilities: slices.Clone(task.Host.Capabilities), Effects: effects, Cache: task.CacheEnabled()}
			}
		}
		dependencies[gi] = sortedInts(dependencies[gi])
	}
	// Explicit contraction may span a success gate. Absorb only the quotient
	// cycles created by that explicit request, preserving acyclic native needs.
	for {
		changed := false
		for _, component := range quotientSCC(union, dependencies) {
			for _, member := range component[1:] {
				changed = union.join(component[0], member) || changed
			}
		}
		if !changed {
			break
		}
	}
	units, err := quotientOrder(union, dependencies)
	if err != nil {
		return nil, err
	}
	if len(units) > MaxGroups {
		return nil, fmt.Errorf("CI generation supports at most %d provider execution units plus setup and aggregate", MaxGroups)
	}
	members := map[int][]int{}
	rootIDs := map[int]string{}
	for gi := range nodes {
		root := union.find(gi)
		members[root] = append(members[root], gi)
	}
	for _, root := range units {
		names := []string{}
		for _, gi := range members[root] {
			names = append(names, nodes[gi].Name)
		}
		rootIDs[root] = unitID(sortedUnique(names))
	}
	for _, root := range units {
		names, execute := []string{}, []string{}
		for _, gi := range members[root] {
			names = append(names, nodes[gi].Name)
			for _, job := range nodes[gi].Jobs {
				execute = append(execute, job.Name)
			}
		}
		names = sortedUnique(names)
		group := Group{ID: rootIDs[root], Name: strings.Join(names, " + "), Members: names, MaxParallel: 1, FailFast: true}
		for _, gi := range members[root] {
			for _, previous := range dependencies[gi] {
				if other := union.find(previous); other != root {
					group.Needs = append(group.Needs, rootIDs[other])
				}
			}
		}
		group.Needs = sortedUnique(group.Needs)
		if len(members[root]) == 1 && !forced[members[root][0]] {
			original := nodes[members[root][0]]
			task, authored := w.Tasks[original.Name]
			group.Matrix = authored && task.Strategy != nil
			group.MaxParallel, group.FailFast = original.MaxParallel, original.FailFast
			for _, variant := range original.Jobs {
				group.Jobs = append(group.Jobs, Job{ID: variant.Name, Task: group.Name, Matrix: maps.Clone(variant.Matrix), Execute: []string{variant.Name}, OutputKey: OutputKey(variant.Name)})
			}
		} else {
			id := group.ID + "--unit"
			group.Jobs = []Job{{ID: id, Task: group.Name, Execute: execute, OutputKey: OutputKey(id)}}
		}
		for ji := range group.Jobs {
			job := &group.Jobs[ji]
			selected := map[string]bool{}
			for _, name := range job.Execute {
				selected[name] = true
			}
			for _, name := range job.Execute {
				for _, source := range dataSources(definitions[name]) {
					job.Data = append(job.Data, source)
					if !selected[source] {
						job.Imports = append(job.Imports, source)
					}
				}
			}
			job.Data = sortedUnique(job.Data)
			job.Imports = sortedUnique(job.Imports)
		}
		p.Groups = append(p.Groups, group)
	}
	owners := map[string]*Job{}
	for gi := range p.Groups {
		for ji := range p.Groups[gi].Jobs {
			job := &p.Groups[gi].Jobs[ji]
			for _, name := range job.Execute {
				owners[name] = job
			}
		}
	}
	for gi := range p.Groups {
		for ji := range p.Groups[gi].Jobs {
			job := &p.Groups[gi].Jobs[ji]
			for _, name := range job.Imports {
				owner := owners[name]
				if owner == nil {
					return nil, fmt.Errorf("missing exact-result producer %q", name)
				}
				owner.Exports = append(owner.Exports, name)
				p.RequiresResults = true
				switch definitions[name].OutputKind() {
				case "component":
					capabilities["component-load"] = true
					capabilities["component-load-name"] = true
				case "image":
					capabilities["execute"] = true
				}
			}
			for _, name := range job.Execute {
				publication := definitions[name].Publish
				if publication != nil && slices.Contains(job.Imports, publication.From) && definitions[publication.From].OutputKind() == "image" {
					capabilities["publish-oci-layout"] = true
				}
			}
		}
	}
	for _, job := range owners {
		job.Exports = sortedUnique(job.Exports)
	}
	if (len(config.CacheFrom) != 0 || len(config.CacheTo) != 0) && len(capabilities) != 0 {
		capabilities["registry-cache"] = true
	}
	if config.Network != "" && len(capabilities) != 0 {
		capabilities["run-network"] = true
	}
	if len(p.Host) != 0 {
		p.Limitations = append(p.Limitations,
			"Host tasks inherit the runner user's authority and require caller opt-in. Declared tools, native platform and capabilities must already be provisioned; imports and generated plans grant no host authority.",
			"Host commands are not assumed hermetic. Host result reuse requires explicit captured effects and input/environment/toolchain/platform declarations; fresh tasks and uncaptured side effects execute.")
		if len(capabilities) == 0 {
			p.Engine.PinStatus = "Coopr is not required for this host-only plan; container execution remains unpinned."
			p.Limitations[1] = "Host-only jobs require Stoke and their declared native platform, tools and capabilities already provisioned on the runner; no Coopr executable is required. Generation installs nothing."
		}
	}
	if config.JobContainerImage != "" {
		p.Limitations[1] = "Every generated job runs in the explicitly selected tools image. The Linux Docker runner, image architecture, runner-owned file access and nested OCI security profile must be qualified separately; generation installs nothing and adds no container options implicitly."
		p.Limitations = append(p.Limitations, "Host tasks execute inside the job container, not on the outer runner. Caller opt-in and capability attestations still apply. Image pull credentials must be available before job steps; workflow registry credentials remain separate.")
	}
	for capability := range capabilities {
		p.Engine.Capabilities = append(p.Engine.Capabilities, capability)
	}
	slices.Sort(p.Engine.Capabilities)
	p.WorkflowDigest, err = digestJSON(semantics)
	if err != nil {
		return nil, err
	}
	p.ID, err = planDigest(p)
	if err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

func sortedInts(values []int) []int { slices.Sort(values); return slices.Compact(values) }

// OutputKey is unique per concrete provider row, avoiding last-writer matrix outputs.
func OutputKey(job string) string {
	sum := sha256.Sum256([]byte(job))
	return fmt.Sprintf("result_%x", sum[:])
}

// InputEnv is a bounded per-slot variable; one giant needs JSON can exceed
// Linux's single environment-string limit even below GitHub's output budget.
func InputEnv(outputKey string) string {
	return "STOKE_CI_INPUT_" + strings.TrimPrefix(outputKey, "result_")
}

// partition contracts existing task groups; it does not plan execution.
// The smallest original index is the stable representative.
type partition struct{ parent []int }

func newPartition(size int) *partition {
	p := &partition{parent: make([]int, size)}
	for i := range p.parent {
		p.parent[i] = i
	}
	return p
}
func (p *partition) find(i int) int {
	if p.parent[i] != i {
		p.parent[i] = p.find(p.parent[i])
	}
	return p.parent[i]
}
func (p *partition) join(a, b int) bool {
	a, b = p.find(a), p.find(b)
	if a == b {
		return false
	}
	if a > b {
		a, b = b, a
	}
	p.parent[b] = a
	return true
}
func quotientEdges(p *partition, dependencies [][]int) map[int][]int {
	edges := map[int][]int{}
	for consumer, predecessors := range dependencies {
		target := p.find(consumer)
		if _, ok := edges[target]; !ok {
			edges[target] = nil
		}
		for _, predecessor := range predecessors {
			source := p.find(predecessor)
			if source != target {
				edges[source] = append(edges[source], target)
			}
		}
	}
	for node := range edges {
		slices.Sort(edges[node])
		edges[node] = slices.Compact(edges[node])
	}
	return edges
}
func quotientSCC(p *partition, dependencies [][]int) [][]int {
	edges := quotientEdges(p, dependencies)
	index, next := 0, map[int]int{}
	low, active := map[int]int{}, map[int]bool{}
	stack, result := []int{}, [][]int{}
	var visit func(int)
	visit = func(node int) {
		index++
		next[node], low[node] = index, index
		stack = append(stack, node)
		active[node] = true
		for _, target := range edges[node] {
			if next[target] == 0 {
				visit(target)
				low[node] = min(low[node], low[target])
			} else if active[target] {
				low[node] = min(low[node], next[target])
			}
		}
		if low[node] == next[node] {
			component := []int{}
			for {
				last := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				active[last] = false
				component = append(component, last)
				if last == node {
					break
				}
			}
			slices.Sort(component)
			if len(component) > 1 {
				result = append(result, component)
			}
		}
	}
	for i := range dependencies {
		if p.find(i) == i && next[i] == 0 {
			visit(i)
		}
	}
	return result
}
func quotientOrder(p *partition, dependencies [][]int) ([]int, error) {
	edges := quotientEdges(p, dependencies)
	indegree := map[int]int{}
	for node := range edges {
		indegree[node] = 0
	}
	for _, targets := range edges {
		for _, target := range targets {
			indegree[target]++
		}
	}
	order := []int{}
	for len(order) < len(edges) {
		candidate := -1
		for node, degree := range indegree {
			if degree == 0 && (candidate < 0 || node < candidate) {
				candidate = node
			}
		}
		if candidate < 0 {
			return nil, errors.New("CI co-location quotient remains cyclic")
		}
		delete(indegree, candidate)
		order = append(order, candidate)
		for _, target := range edges[candidate] {
			indegree[target]--
		}
	}
	return order, nil
}
func unitID(members []string) string {
	if len(members) == 1 {
		return providerID(members[0])
	}
	return providerID("unit:" + strings.Join(members, "\x00"))
}

func (p *Plan) FindJob(id string) (Job, bool) {
	if p != nil {
		for _, group := range p.Groups {
			for _, job := range group.Jobs {
				if job.ID == id {
					return job, true
				}
			}
		}
	}
	return Job{}, false
}

func (p *Plan) Validate() error {
	if p == nil || p.Version != Version || p.GeneratedBy != "stoke-ci" {
		return errors.New("unsupported or unmanaged Stoke CI plan")
	}
	config, err := normalizeConfig(p.Config)
	if err != nil {
		return err
	}
	before, _ := json.Marshal(p.Config)
	after, _ := json.Marshal(config)
	if !bytes.Equal(before, after) {
		return errors.New("CI plan configuration is not canonical")
	}
	if len(p.Groups) == 0 || len(p.Groups) > MaxGroups || !validDigest(p.WorkflowDigest) || p.Engine.APIVersion != cooprwire.Version || p.Engine.ProductionPin != nil {
		return errors.New("invalid CI plan graph or engine contract")
	}
	type owner struct {
		group int
		job   *Job
	}
	providers, selections, logicalMembers, outputKeys := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	providerRows := map[string][]Job{}
	owners := map[string]owner{}
	totalOutputs := 0
	for gi := range p.Groups {
		group := &p.Groups[gi]
		if len(group.Members) == 0 || !slices.Equal(group.Members, sortedUnique(group.Members)) || group.ID != unitID(group.Members) || group.Name != strings.Join(group.Members, " + ") || providers[group.ID] || len(group.Jobs) == 0 || len(group.Jobs) > workflow.MaxVariants || group.MaxParallel < 1 || group.MaxParallel > workflow.MaxVariants || group.Matrix && len(group.Members) != 1 || !group.Matrix && len(group.Jobs) != 1 {
			return errors.New("invalid CI execution unit")
		}
		for _, member := range group.Members {
			if member == "" || logicalMembers[member] {
				return errors.New("duplicate CI logical task membership")
			}
			logicalMembers[member] = true
		}
		if !slices.Equal(group.Needs, sortedUnique(group.Needs)) {
			return errors.New("CI prerequisites are not canonical")
		}
		inputSize := 4096
		for _, dependency := range group.Needs {
			for _, parent := range providerRows[dependency] {
				inputSize += MaxNativeMetadataBytes + len(InputEnv(parent.OutputKey)) + 2
			}
		}
		if inputSize > maxNativeInputEnvBytes {
			return errors.New("CI native input metadata exceeds the 1 MiB environment budget; explicitly co-locate part of the fan-in")
		}
		for _, dependency := range group.Needs {
			if !providers[dependency] {
				return errors.New("CI unit prerequisites are absent or unordered")
			}
		}
		outputSize := 0
		for ji := range group.Jobs {
			job := &group.Jobs[ji]
			if job.ID == "" || len(job.ID) > 512 || job.Task != group.Name || selections[job.ID] || len(job.Execute) == 0 || len(job.Execute) > workflow.MaxJobs || job.OutputKey != OutputKey(job.ID) || outputKeys[job.OutputKey] {
				return errors.New("invalid, oversized or duplicate CI selection/output key")
			}
			selections[job.ID], outputKeys[job.OutputKey] = true, true
			outputSize += 2 * (len(job.OutputKey) + MaxNativeMetadataBytes + 4)
			selected := map[string]bool{}
			for _, name := range job.Execute {
				if name == "" || selected[name] {
					return errors.New("CI primary execution is empty or duplicated")
				}
				if _, exists := owners[name]; exists {
					return errors.New("CI primary execution is duplicated")
				}
				selected[name] = true
				owners[name] = owner{gi, job}
			}
			for _, names := range [][]string{job.Data, job.Imports, job.Exports} {
				if !slices.Equal(names, sortedUnique(names)) {
					return errors.New("CI data/result selections are not canonical")
				}
			}
			for _, name := range job.Data {
				if !selected[name] && !slices.Contains(job.Imports, name) {
					return errors.New("CI data source has no exact imported result")
				}
			}
			for _, name := range job.Imports {
				if selected[name] || !slices.Contains(job.Data, name) {
					return errors.New("invalid CI exact-result import")
				}
			}
			for _, name := range job.Exports {
				if !selected[name] {
					return errors.New("CI result export is not executed by its owner")
				}
			}
		}
		if outputSize > maxNativeOutputBytes {
			return errors.New("matrix result metadata exceeds GitHub's 1 MiB job-output bound")
		}
		totalOutputs += outputSize
		providers[group.ID] = true
		providerRows[group.ID] = group.Jobs
	}
	if len(owners) > workflow.MaxJobs || totalOutputs > maxWorkflowOutputBytes {
		return errors.New("CI plan exceeds expanded-job or native output limit")
	}
	for name, requirement := range p.Host {
		if _, exists := owners[name]; !exists || len(requirement.Tools) == 0 ||
			(requirement.Platform != "linux/amd64" && requirement.Platform != "linux/arm64") ||
			(requirement.Effects != "external" && requirement.Effects != "captured") ||
			(requirement.Cache && requirement.Effects != "captured") {
			return errors.New("invalid host provisioning metadata in CI plan")
		}
	}
	required := false
	consumed := map[string]bool{}
	for gi, group := range p.Groups {
		for _, job := range group.Jobs {
			for _, name := range job.Imports {
				producer, exists := owners[name]
				if !exists || producer.group >= gi || !slices.Contains(producer.job.Exports, name) || !slices.Contains(group.Needs, p.Groups[producer.group].ID) {
					return errors.New("CI exact-result import lacks an earlier native dependency/export")
				}
				required = true
				consumed[name] = true
			}
		}
	}
	for _, producer := range owners {
		for _, name := range producer.job.Exports {
			if !consumed[name] {
				return errors.New("CI result export has no consumer")
			}
		}
	}
	if p.RequiresResults != required {
		return errors.New("CI result repository requirement does not match the graph")
	}
	for name, consumer := range p.Environments {
		if !strings.HasPrefix(name, "stoke-environment-") || consumer == "" || consumer == name {
			return errors.New("invalid synthetic CI environment")
		}
		producer, ok := owners[name]
		if !ok || !slices.Contains(producer.job.Exports, name) {
			return errors.New("synthetic CI environment has no exported producer")
		}
		if _, ok := owners[consumer]; !ok {
			return errors.New("synthetic CI environment representative is absent")
		}
	}
	expected, err := planDigest(p)
	if err != nil || !validDigest(p.ID) || p.ID != expected {
		return errors.New("CI plan content digest mismatch; regenerate the plan")
	}
	return nil
}

func Encode(p *Plan) ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, err
	}
	if len(data)+1 > MaxPlanBytes {
		return nil, errors.New("CI plan exceeds 4 MiB")
	}
	return append(data, '\n'), nil
}

func Decode(r io.Reader) (*Plan, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxPlanBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxPlanBytes {
		return nil, errors.New("CI plan exceeds 4 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var p Plan
	if err := decoder.Decode(&p); err != nil {
		return nil, fmt.Errorf("CI plan: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("CI plan requires exactly one JSON document")
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

func normalizeConfig(config Config) (Config, error) {
	if config.JobContainerImage != "" {
		ref, err := registry.ParseReference(config.JobContainerImage)
		if err != nil || len(config.JobContainerImage) > 512 || strings.Contains(config.JobContainerImage, "${{") || ref.ValidateReferenceAsDigest() != nil || config.JobContainerImage != ref.Registry+"/"+ref.Repository+"@"+ref.Reference {
			return config, errors.New("job container image requires a literal digest-pinned registry reference of at most 512 bytes")
		}
	}
	if config.JobContainerOptions != "" {
		if config.JobContainerImage == "" {
			return config, errors.New("job container options require a job container image")
		}
		if len(config.JobContainerOptions) > 4096 || !utf8.ValidString(config.JobContainerOptions) || strings.ContainsAny(config.JobContainerOptions, "\x00\r\n") || strings.Contains(config.JobContainerOptions, "${{") {
			return config, errors.New("job container options must be literal single-line text of at most 4096 bytes")
		}
	}
	if len(config.Network) > 4096 || !utf8.ValidString(config.Network) || strings.ContainsAny(config.Network, "\x00\r\n") {
		return config, errors.New("network setting must be bounded printable text")
	}
	if config.Workflow == "" {
		config.Workflow = "stoke.yaml"
	}
	if config.Context == "" {
		config.Context = path.Dir(config.Workflow)
	}
	for _, location := range []struct{ name, value string }{{"workflow", config.Workflow}, {"context", config.Context}} {
		if err := relativePath(location.value, location.name == "context"); err != nil {
			return config, fmt.Errorf("CI %s: %w", location.name, err)
		}
	}
	config.Targets = sortedUnique(config.Targets)
	config.CoLocate = sortedUnique(config.CoLocate)
	if config.ResultsRepository != "" {
		if len(config.ResultsRepository) > 512 || strings.Contains(config.ResultsRepository, "${{") {
			return config, errors.New("results repository must be a literal bare registry repository of at most 512 bytes")
		}
		if err := engine.ValidateCacheRepository(config.ResultsRepository); err != nil {
			return config, fmt.Errorf("results repository: %w", err)
		}
	}
	config.Inputs = maps.Clone(config.Inputs)
	config.CacheFrom = slices.Clone(config.CacheFrom)
	config.CacheTo = slices.Clone(config.CacheTo)
	for _, direction := range []struct {
		name         string
		repositories []string
	}{{"cache-from", config.CacheFrom}, {"cache-to", config.CacheTo}} {
		for _, repository := range direction.repositories {
			if strings.Contains(repository, "${{") {
				return config, fmt.Errorf("%s must be a literal registry cache repository", direction.name)
			}
			if err := engine.ValidateCacheRepository(repository); err != nil {
				return config, fmt.Errorf("%s: %w", direction.name, err)
			}
		}
	}
	config.Runners = slices.Clone(config.Runners)
	if len(config.Runners) == 0 {
		config.Runners = []string{"self-hosted", "linux"}
	}
	for _, label := range config.Runners {
		if label == "" || len(label) > 256 || !utf8.ValidString(label) || strings.ContainsAny(label, "\x00\r\n") || strings.Contains(label, "${{") {
			return config, errors.New("CI runner labels must be nonempty literal strings of at most 256 bytes")
		}
	}
	if config.TimeoutMinutes == 0 {
		config.TimeoutMinutes = 45
	}
	if config.TimeoutMinutes < 1 || config.TimeoutMinutes > 360 {
		return config, errors.New("CI timeout must be between 1 and 360 minutes")
	}
	return config, nil
}

func relativePath(value string, directory bool) error {
	if value == "" || !utf8.ValidString(value) || path.IsAbs(value) || path.Clean(value) != value || value == ".." || strings.HasPrefix(value, "../") || strings.ContainsAny(value, "\\\x00\r\n") || strings.Contains(value, "://") || strings.Contains(value, "${{") || (!directory && value == ".") {
		return errors.New("requires a canonical checkout-relative path without expressions")
	}
	return nil
}

func portableTask(task workflow.Task) error {
	for name, directory := range task.Contexts {
		if err := relativePath(directory, true); err != nil {
			return fmt.Errorf("named context %q: %w", name, err)
		}
	}
	for _, mount := range task.AllMounts() {
		if mount.Type == "host" {
			return errors.New("live host mounts are not portable between CI jobs; use declared context or image outputs")
		}
	}
	return nil
}

// Dependencies already owns every implicit artifact edge. Clear only explicit
// success gates to distinguish materialized data from provider-only gates.
func dataSources(task workflow.Task) []string {
	task.Needs = nil
	return task.Dependencies()
}

type assetDigest struct {
	Name   string `json:"name"`
	Mode   uint32 `json:"mode"`
	SHA256 string `json:"sha256"`
}

func componentAssets(directory string) (_ []assetDigest, retErr error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, root.Close()) }()
	files := []assetDigest{}
	total := int64(0)
	err = filepath.WalkDir(directory, func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("component assets must be regular files")
		}
		if len(files) >= assets.MaxFiles {
			return errors.New("component asset file limit exceeded")
		}
		relative, err := filepath.Rel(directory, file)
		if err != nil {
			return err
		}
		fileHandle, err := root.OpenFile(relative, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		info, err := fileHandle.Stat()
		if err != nil {
			return errors.Join(err, fileHandle.Close())
		}
		if !info.Mode().IsRegular() || info.Size() > assets.MaxFileBytes {
			return errors.Join(errors.New("component asset must be a bounded regular file"), fileHandle.Close())
		}
		hash := sha256.New()
		size, readErr := io.Copy(hash, io.LimitReader(fileHandle, assets.MaxFileBytes+1))
		if err := errors.Join(readErr, fileHandle.Close()); err != nil {
			return err
		}
		total += size
		if size > assets.MaxFileBytes || total > assets.MaxTotalBytes {
			return errors.New("component asset size limit exceeded")
		}
		files = append(files, assetDigest{Name: filepath.ToSlash(relative), Mode: uint32(info.Mode().Perm()), SHA256: hex.EncodeToString(hash.Sum(nil))})
		return nil
	})
	return files, err
}

func providerID(name string) string {
	sum := sha256.Sum256([]byte(name))
	return fmt.Sprintf("stoke_%x", sum[:8])
}

func sortedUnique(values []string) []string {
	values = slices.Clone(values)
	slices.Sort(values)
	return slices.Compact(values)
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func digestJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func planDigest(p *Plan) (string, error) {
	copy := *p
	copy.ID = ""
	return digestJSON(copy)
}
