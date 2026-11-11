package main

import (
	"strings"
	"testing"

	"github.com/strio-dev/stoke/internal/ci"
	"github.com/strio-dev/stoke/internal/workflow"
)

func TestCIProducerSpecsDistinguishNativeDefaultsFromRequiredPlatforms(t *testing.T) {
	w, err := workflow.Decode(strings.NewReader(`version: 1
tasks:
  native: {coopr: {files: [image.coopr]}, platform: linux/amd64}
  component: {coopr: {files: [image.coopr], kind: component}, platform: linux/amd64}
  literal: {image: scratch, platform: linux/amd64}
  host:
    platform: linux/amd64
    host: {tools: [sh], outputs: [out]}
    steps: [{run: [sh, -c, true]}]
  inline: {environment: {files: [image.coopr]}, platform: linux/amd64}
`))
	if err != nil {
		t.Fatal(err)
	}
	// Include an exported promoted environment as well as its literal consumer.
	p := &ci.Plan{
		Environments: map[string]string{"stoke-environment-shared": "inline"},
		Groups:       []ci.Group{{Jobs: []ci.Job{{ID: "producer-unit", Exports: []string{"native", "component", "literal", "host", "inline", "stoke-environment-shared"}}}}},
	}
	specs, err := ciProducerSpecs(w, p)
	if err != nil {
		t.Fatal(err)
	}
	for name, expected := range map[string]ciProducerSpec{
		"native":                   {Kind: "image"},
		"component":                {Kind: "component"},
		"literal":                  {Kind: "image", Platform: "linux/amd64"},
		"host":                     {Kind: "host", Platform: "linux/amd64"},
		"inline":                   {Kind: "image", Platform: "linux/amd64"},
		"stoke-environment-shared": {Kind: "image"},
	} {
		expected.Unit = "producer-unit"
		if specs[name] != expected {
			t.Errorf("%s expectation = %+v, want %+v", name, specs[name], expected)
		}
	}
}
