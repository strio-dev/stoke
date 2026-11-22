package workflow

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestTaskBuildSchema(t *testing.T) {
	w, err := Decode(strings.NewReader(`version: 1
tasks:
  check:
    build: {coopr: [second.coopr, first.coopr], context: ./service, target: tools, args: {MODE: debug}}
    env: {MODE: override}
    steps: [{run: [check]}]
`))
	if err != nil {
		t.Fatal(err)
	}
	b := w.Tasks["check"].Environment
	if !reflect.DeepEqual(b.Files, []string{"second.coopr", "first.coopr"}) || b.Target != "tools" {
		t.Fatalf("build order/target lost: %+v", b)
	}
	data, err := yaml.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Decode(strings.NewReader(string(data)))
	if err != nil || !reflect.DeepEqual(w, again) || !strings.Contains(string(data), "coopr:") || strings.Contains(string(data), "environment:") || strings.Contains(string(data), "files:") {
		t.Fatalf("build YAML roundtrip: %s %v", data, err)
	}
}

func TestTaskBuildRejectsOtherShapes(t *testing.T) {
	for _, body := range []string{
		"environment: {files: [image.coopr]}",
		"build: {files: [image.coopr]}",
		"build: {dockerfile: Dockerfile}",
		"build: {coopr: image.coopr}",
		"build: {coopr: []}",
		"build: {coopr: [base.coopr, null]}",
		"build: {coopr: [base.coopr, false]}",
		"build: {coopr: [42]}",
		"build: {coopr: [image.coopr], context: ../outside}",
		"build: {coopr: [image.coopr], context: /outside}",
		"build: {coopr: [image.coopr], context: null}",
		"build: {coopr: [image.coopr], context: false}",
		"build: {coopr: [image.coopr], kind: component}",
		"build: {coopr: [image.coopr]}, image: scratch",
		"build: {coopr: [image.coopr]}, from: other",
		"build: {coopr: [image.coopr]}, coopr: {files: [image.coopr]}",
		"build: null, image: scratch",
	} {
		if _, err := Decode(strings.NewReader("version: 1\ntasks: {other: {image: scratch}, check: {" + body + "}}")); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
}

func TestTaskBuildDefaultWireShape(t *testing.T) {
	w, err := Decode(strings.NewReader("version: 1\ntasks: {check: {build: {coopr: [image.coopr]}}}"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(w.Tasks["check"].Environment)
	if err != nil || string(data) != `{"Files":["image.coopr"],"FilesFrom":"","Kind":"","Target":"","Args":null}` {
		t.Fatalf("default build changed established wire identity: %s %v", data, err)
	}
}
