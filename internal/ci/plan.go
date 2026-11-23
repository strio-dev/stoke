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
)

const Version = 5
const MaxPlanBytes = 4 << 20

type Config struct {
	Workflow  string            `json:"workflow"`
	Context   string            `json:"context"`
	Targets   []string          `json:"targets,omitempty"`
	Inputs    map[string]string `json:"inputs,omitempty"`
	CacheFrom []string          `json:"cacheFrom,omitzero"`
	CacheTo   []string          `json:"cacheTo,omitzero"`
	CoLocate  []string          `json:"coLocate,omitempty"`
	Network   string            `json:"network,omitempty"`
}

type Engine struct {
	APIVersion    int      `json:"apiVersion"`
	Capabilities  []string `json:"capabilities"`
	ProductionPin *string  `json:"productionPin"`
	PinStatus     string   `json:"pinStatus"`
}

type Plan struct {
	Version        int                        `json:"version"`
	GeneratedBy    string                     `json:"generatedBy"`
	ID             string                     `json:"id"`
	Config         Config                     `json:"config"`
	WorkflowDigest string                     `json:"workflowDigest"`
	Engine         Engine                     `json:"engine"`
	Groups         []Group                    `json:"groups"`
	Limitations    []string                   `json:"limitations"`
	Host           map[string]HostRequirement `json:"host,omitempty"`
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
	ID      string            `json:"id"`
	Task    string            `json:"task"`
	Matrix  map[string]string `json:"matrix,omitempty"`
	Execute []string          `json:"execute"`
}

var limitations = []string{
	"The CI provider owns scheduling and success gates; independent jobs may continue after another job fails.",
	fmt.Sprintf("Runners must already provide Stoke, compatible Coopr API v%d, Linux rootless execution and any required binfmt/QEMU; generation installs nothing.", cooprwire.Version),
	"Local workflow/context selectors must be checkout-relative; OCI workflow imports remain supported, but OCI root selectors and live host mounts are not generated.",
	"Native jobs convey success gates. An external data producer must be restored from matching Coopr cache or the job fails.",
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
	p := &Plan{Version: Version, GeneratedBy: "stoke-ci", Config: config, Limitations: slices.Clone(limitations)}
	p.Engine = Engine{APIVersion: cooprwire.Version, PinStatus: "Unpinned: supply a compatible explicitly selected executable; production distribution remains separate."}
	nodes := slices.Clone(expanded.Groups)
	logical, concrete := map[string]int{}, map[string]int{}
	definitions := map[string]workflow.Task{}
	for gi, group := range nodes {
		logical[group.Name] = gi
		for _, job := range group.Jobs {
			if len(job.Name) > 512 {
				return nil, fmt.Errorf("concrete CI task name exceeds the 512-byte selector limit")
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
			semantics = append(semantics, semanticJob{Name: job.Name, Definition: semantic, Assets: files, MaxParallel: group.MaxParallel, FailFast: group.FailFast})
			platform, err := task.ResolvedPlatform()
			if err != nil {
				return nil, err
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
				group.Jobs = append(group.Jobs, Job{ID: variant.Name, Task: group.Name, Matrix: maps.Clone(variant.Matrix), Execute: []string{variant.Name}})
			}
		} else {
			id := group.ID
			group.Jobs = []Job{{ID: id, Task: group.Name, Execute: execute}}
		}
		p.Groups = append(p.Groups, group)
	}
	for _, consumers := range shared {
		first := concrete[consumers[0]]
		root := union.find(first)
		for _, name := range consumers {
			if union.find(concrete[name]) != root || len(members[root]) == 1 && !forced[first] && len(nodes[first].Jobs) > 1 {
				return nil, fmt.Errorf("shared inline environment spans CI jobs; use --co-locate %s to build it in one invocation", nodes[first].Name)
			}
		}
	}
	cacheFrom, cacheTo := w.CacheRepositories(config.CacheFrom, config.CacheTo)
	for _, group := range p.Groups {
		for _, job := range group.Jobs {
			request := runner.CIRequest{Targets: config.Targets, Execute: job.Execute}
			required, err := runner.RequiredCapabilities(w, expanded, &request, runner.Options{Network: config.Network, CacheFrom: cacheFrom, CacheTo: cacheTo})
			if err != nil {
				return nil, err
			}
			for _, capability := range required {
				capabilities[capability] = true
			}
		}
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
	for capability := range capabilities {
		p.Engine.Capabilities = append(p.Engine.Capabilities, capability)
	}
	slices.Sort(p.Engine.Capabilities)
	p.WorkflowDigest, err = digestJSON(struct {
		Jobs      []semanticJob `json:"jobs"`
		CacheFrom []string      `json:"cacheFrom,omitempty"`
		CacheTo   []string      `json:"cacheTo,omitempty"`
	}{semantics, w.CacheFrom, w.CacheTo})
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
	// A task belongs to exactly one execution unit. Co-located units use
	// their first member in canonical order as their logical name.
	return members[0]
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
	if len(p.Groups) == 0 || len(p.Groups) > workflow.MaxJobs || !validDigest(p.WorkflowDigest) || p.Engine.APIVersion != cooprwire.Version || p.Engine.ProductionPin != nil {
		return errors.New("invalid CI plan graph or engine contract")
	}
	providers, selections, logicalMembers := map[string]bool{}, map[string]bool{}, map[string]bool{}
	owners := map[string]bool{}
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
		for _, dependency := range group.Needs {
			if !providers[dependency] {
				return errors.New("CI unit prerequisites are absent or unordered")
			}
		}
		for ji := range group.Jobs {
			job := &group.Jobs[ji]
			if job.ID == "" || len(job.ID) > 512 || job.Task != group.Name || selections[job.ID] || len(job.Execute) == 0 || len(job.Execute) > workflow.MaxJobs {
				return errors.New("invalid, oversized or duplicate CI selection")
			}
			selections[job.ID] = true
			selected := map[string]bool{}
			for _, name := range job.Execute {
				if name == "" || selected[name] {
					return errors.New("CI primary execution is empty or duplicated")
				}
				if _, exists := owners[name]; exists {
					return errors.New("CI primary execution is duplicated")
				}
				selected[name] = true
				owners[name] = true
			}

		}
		providers[group.ID] = true
	}
	if len(owners) > workflow.MaxJobs {
		return errors.New("CI plan exceeds expanded-job limit")
	}
	for name, requirement := range p.Host {
		if _, exists := owners[name]; !exists || len(requirement.Tools) == 0 ||
			(requirement.Platform != "linux/amd64" && requirement.Platform != "linux/arm64") ||
			(requirement.Effects != "external" && requirement.Effects != "captured") ||
			(requirement.Cache && requirement.Effects != "captured") {
			return errors.New("invalid host provisioning metadata in CI plan")
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
