package workflow

import (
	"strings"
	"testing"
)

func TestRootRegistryCacheYAML(t *testing.T) {
	for _, tc := range []struct {
		policy string
		valid  bool
	}{
		{"cache-from: [registry.example/read, localhost:5000/cache]\ncache-to: [registry.example/write]\n", true},
		{"cache-from: []\ncache-to: []\n", true},
		{"cache-from: null\n", false},
		{"cache-to: registry.example/write\n", false},
		{"cache-from: [false]\n", false},
		{"cache-to: [registry.example/cache:tag]\n", false},
		{"cache-from: ['']\n", false},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			_, err := Decode(strings.NewReader("version: 1\n" + tc.policy + "tasks: {build: {image: scratch}}\n"))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
}
