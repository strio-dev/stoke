package artifact

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/workflow"
)

const CIComponentArchiveType = "application/vnd.strio.stoke.ci.component.archive.v1+tar"

const (
	CIResultType          = "application/vnd.strio.stoke.ci.result.v2"
	CIInputSetType        = "application/vnd.strio.stoke.ci.inputs.v2"
	ciResultConfigType    = CIResultType + "+json"
	ciInputSetConfigType  = CIInputSetType + "+json"
	CIVersion             = 2
	MaxCIBindings         = 4096
	MaxCIMetadataBytes    = 4 << 20
	ciComponentType       = "application/vnd.coopr.strio.dev.component.v1+json"
	ciComponentConfigType = "application/vnd.coopr.strio.dev.component.config.v1+json"
)

// CIScope identifies one reusable-workflow invocation, not a security principal.
// Attempts belong to individual results so explicit prior-attempt inputs can be
// reused without looking up mutable tags or guessing a "latest" execution.
type CIScope struct {
	Provider   string `json:"provider"`
	Server     string `json:"server"`
	Repository string `json:"repository"`
	Run        string `json:"run"`
	Source     string `json:"source"`
	Invocation string `json:"invocation"`
	Plan       string `json:"plan"`
}

func (s CIScope) Validate() error {
	for name, value := range map[string]string{
		"provider": s.Provider, "server": s.Server, "repository": s.Repository,
		"run": s.Run, "source": s.Source, "invocation": s.Invocation, "plan": s.Plan,
	} {
		if !ciText(value, 512) {
			return fmt.Errorf("CI scope %s must be nonempty bounded text without control characters", name)
		}
	}
	if len(s.Plan) != sha256.Size*2 {
		return errors.New("CI scope plan must be a SHA256 hex fingerprint")
	}
	if _, err := hex.DecodeString(s.Plan); err != nil {
		return errors.New("CI scope plan must be a SHA256 hex fingerprint")
	}
	return nil
}

func (s CIScope) Hash() (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// NewCIExecutionID creates an execution identity even when payload bytes happen
// to be identical. It is uniqueness metadata, not an authentication token.
func NewCIExecutionID() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(nonce[:]), nil
}

// CIInputSet describes consumed producer executions. It contains no success or
// status field and can be published or forwarded before any gate runs.
type CIInputSet struct {
	Version  int               `json:"version"`
	Scope    CIScope           `json:"scope"`
	Bindings map[string]string `json:"bindings"`
}

// CIResult selects canonical exported OCI bytes, independently of local storage
// handles and any representation used when importing into a native store.
type CIResult struct {
	Version   int            `json:"version"`
	Scope     CIScope        `json:"scope"`
	Producer  string         `json:"producer"`
	Attempt   int            `json:"attempt"`
	Execution string         `json:"execution"`
	Kind      string         `json:"kind"`
	Platform  string         `json:"platform,omitempty"`
	Output    v1.Descriptor  `json:"output"`
	Inputs    string         `json:"inputs,omitempty"`
	Archive   *v1.Descriptor `json:"archive,omitempty"`
}

type CIResultExpectation struct {
	Scope      CIScope
	Producer   string
	MaxAttempt int
	Kind       string
	Platform   string
}

// CIPayload identifies a verified exact result. Resolved image inputs have only
// CanonicalReference; fetched payloads have a caller-owned OCI layout or archive
// in Path. Captured host files and components are always fetched in full.
type CIPayload struct {
	Result             CIResult
	Path               string
	CanonicalReference string
}

func ValidateCIRepository(repository string) error {
	if len(repository) > 512 {
		return errors.New("CI result repository exceeds 512 bytes")
	}
	return engine.ValidateCacheRepository(repository)
}

func ciText(value string, limit int) bool {
	return value != "" && len(value) <= limit && utf8.ValidString(value) &&
		strings.TrimSpace(value) == value && !strings.ContainsFunc(value, unicode.IsControl)
}

func ciDescriptor(d v1.Descriptor) error {
	if d.Digest.Validate() != nil || d.Digest.Algorithm() != digest.SHA256 ||
		d.Size < 0 || d.Size > 1<<40 || d.MediaType == "" ||
		len(d.URLs) != 0 || len(d.Data) != 0 {
		return errors.New("invalid CI OCI descriptor")
	}
	return nil
}

func (r CIResult) validate(expect CIResultExpectation) error {
	if r.Version != CIVersion {
		return errors.New("unsupported CI result version")
	}
	if err := r.Scope.Validate(); err != nil {
		return err
	}
	if r.Scope != expect.Scope {
		return errors.New("CI result scope/source/plan mismatch")
	}
	if !ciText(r.Producer, 512) || expect.Producer != "" && r.Producer != expect.Producer {
		return errors.New("CI result producer mismatch")
	}
	if r.Attempt < 1 || expect.MaxAttempt > 0 && r.Attempt > expect.MaxAttempt {
		return errors.New("CI result has invalid or future producer attempt")
	}
	if len(r.Execution) != 32 {
		return errors.New("CI result requires execution nonce")
	}
	if _, err := hex.DecodeString(r.Execution); err != nil {
		return errors.New("CI result has invalid execution nonce")
	}
	if r.Kind != "image" && r.Kind != "component" && r.Kind != "host" || expect.Kind != "" && r.Kind != expect.Kind {
		return errors.New("CI result output kind mismatch")
	}
	if r.Platform != "" {
		value, err := workflow.NormalizePlatform(r.Platform)
		if err != nil || value != r.Platform {
			return errors.New("CI result platform must be canonical")
		}
	} else {
		return errors.New("CI result requires actual payload platform")
	}
	if expect.Platform != "" && r.Platform != expect.Platform {
		return errors.New("CI result platform mismatch")
	}
	if err := ciDescriptor(r.Output); err != nil {
		return err
	}
	if r.Kind == "component" {
		if r.Archive == nil || r.Archive.MediaType != CIComponentArchiveType {
			return errors.New("CI component result requires original OCI archive")
		}
		if err := ciDescriptor(*r.Archive); err != nil {
			return err
		}
	} else if r.Archive != nil {
		return errors.New("CI image result must not include component archive")
	}
	if r.Output.MediaType != v1.MediaTypeImageManifest {
		return errors.New("CI result output must be a single OCI manifest")
	}
	return nil
}

func MergeCIBindings(sets ...CIInputSet) (map[string]string, error) {
	bindings := map[string]string{}
	for _, set := range sets {
		for name, ref := range set.Bindings {
			if !ciText(name, 512) {
				return nil, errors.New("invalid CI binding producer")
			}
			if previous, exists := bindings[name]; exists && previous != ref {
				return nil, fmt.Errorf("CI producer %q has conflicting execution results; rerun producer and dependent gates", name)
			}
			bindings[name] = ref
		}
	}
	if len(bindings) > MaxCIBindings {
		return nil, errors.New("too many CI input bindings")
	}
	return maps.Clone(bindings), nil
}
