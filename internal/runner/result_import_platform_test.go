package runner

import (
	"context"
	"strings"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	coopr "github.com/strio-dev/stoke/internal/engine"
)

func ciARMImageFixture(t *testing.T) CIImport {
	t.Helper()
	input := ciImageFixture(t, false, "EXACT=arm64")
	image, err := readCIImage(t.Context(), input.OCI, input.Digest, input.Platform)
	if err != nil {
		t.Fatal(err)
	}
	image.config.Architecture = "arm64"
	image.manifest.Config = writeCIBlob(t, input.OCI, v1.MediaTypeImageConfig, ciJSON(t, image.config))
	root := writeCIBlob(t, input.OCI, v1.MediaTypeImageManifest, ciJSON(t, image.manifest))
	writeCIIndex(t, input.OCI, []v1.Descriptor{root})
	input.Digest, input.Platform = root.Digest.String(), "linux/arm64"
	input.CanonicalReference = "registry.invalid/results@" + input.Digest
	return input
}

func TestCINativeImportUsesActualPlatformAndPreservesConsumerRequirements(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "inherit", true: "explicit-conflict"}[explicit], func(t *testing.T) {
			platform := ""
			if explicit {
				platform = "platform: linux/amd64, "
			}
			w := fixture(t, "version: 1\ntasks:\n  producer: {coopr: {files: [image.coopr]}, platform: linux/amd64}\n  test: {from: producer, "+platform+"steps: [{run: [check]}]}\n")
			input := ciARMImageFixture(t)
			imports, consumers := 0, 0
			report, err := runCI(t.Context(), w, CIRequest{Execute: []string{"test"}, Imports: map[string]CIImport{"producer": input}}, nativeOptions(t), fakeEngine{execute: func(_ context.Context, p coopr.Program, o coopr.Options) (coopr.Result, error) {
				if strings.HasPrefix(string(p.Output), "stoke-ci-import-") {
					imports++
					if p.Platform != "linux/arm64" {
						t.Errorf("import lost actual platform: %+v", p)
					}
					return importedCIImage(t, input, o), nil
				}
				consumers++
				if p.Platform != "linux/arm64" || p.States[0].From.Image != "containers-storage:imported-exact" {
					t.Errorf("consumer lost actual producer image/platform: %+v", p)
				}
				return coopr.Result{ImageID: "tested", Platform: p.Platform}, nil
			}})
			if imports != 1 {
				t.Fatalf("native import rejected or replayed: imports=%d err=%v", imports, err)
			}
			if explicit {
				if err == nil || !strings.Contains(err.Error(), "conflicts") || consumers != 0 || report.Tasks[0].Status != "failed" {
					t.Fatalf("explicit conflict accepted: consumers=%d report=%+v err=%v", consumers, report, err)
				}
			} else if err != nil || consumers != 1 || report.Status != "passed" || report.Tasks[0].Platform != "linux/arm64" {
				t.Fatalf("actual platform not inherited: consumers=%d report=%+v err=%v", consumers, report, err)
			}
		})
	}
}

func TestCIImportPlatformChecksRetainLiteralAndOCIMetadataRequirements(t *testing.T) {
	input := ciARMImageFixture(t)
	w := fixture(t, "version: 1\ntasks:\n  literal: {image: scratch, platform: linux/amd64}\n  native: {coopr: {files: [image.coopr]}, platform: linux/amd64}\n")
	if err := validateCIImport(t.Context(), input, w.Tasks["literal"]); err == nil || !strings.Contains(err.Error(), "differs from producer platform") {
		t.Fatalf("literal producer accepted different platform: %v", err)
	}
	input.Platform = "linux/amd64"
	if err := validateCIImport(t.Context(), input, w.Tasks["native"]); err == nil || !strings.Contains(err.Error(), "configuration differs") {
		t.Fatalf("native producer accepted mislabeled OCI configuration: %v", err)
	}
}

func TestCIPromotedNativeEnvironmentAcceptsActualPlatform(t *testing.T) {
	w := fixture(t, "version: 1\ntasks:\n  test: {environment: {files: [image.coopr]}, platform: linux/amd64}\n")
	const producer = "stoke-environment-shared"
	request := CIRequest{Execute: []string{"test"}, Environments: map[string]string{producer: "test"}, Imports: map[string]CIImport{producer: ciARMImageFixture(t)}}
	// The promoted native producer may override its default. Preparing the
	// import must succeed before the consumer checks its explicit requirement.
	plan, _, err := prepareCI(t.Context(), w, request, nativeOptions(t))
	if err != nil || plan == nil {
		t.Fatalf("promoted native producer's actual platform rejected: %v", err)
	}
}
