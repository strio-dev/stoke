package engine

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	cooprwire "github.com/strio-dev/coopr/api"
)

func TestLoadComponentCapabilityAndWire(t *testing.T) {
	for _, name := range []string{"", "first:delivery"} {
		for _, supported := range []bool{false, true} {
			t.Run(name+"/"+map[bool]string{false: "missing", true: "supported"}[supported], func(t *testing.T) {
				posts := 0
				load := cooprwire.ComponentLoad{Archive: "/artifacts/component.tar", ComponentStoreDir: "/state/components", Name: name}
				required := "component-load"
				reference := "sha256:verified"
				if name != "" {
					required, reference = "component-load-name", name+"@"+reference
				}
				c := rawSocketClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						info := cooprwire.Info{APIVersion: 1}
						if name != "" {
							info.Capabilities = []string{"component-load"}
						}
						if supported {
							info.Capabilities = append(info.Capabilities, required)
						}
						_ = json.NewEncoder(w).Encode(info)
						return
					}
					posts++
					var got cooprwire.Request
					if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
						t.Error(err)
					}
					if got.Kind != "component-load" || got.ComponentLoad == nil || *got.ComponentLoad != load {
						t.Errorf("wire changed: %+v", got)
					}
					_ = json.NewEncoder(w).Encode(cooprwire.Event{Type: "result", Result: &cooprwire.Result{Reference: reference}})
				}))
				got, err := c.LoadComponent(t.Context(), load, Options{})
				if !supported {
					if err == nil || !strings.Contains(err.Error(), required) || posts != 0 {
						t.Fatalf("preflight err=%v posts=%d", err, posts)
					}
					return
				}
				if err != nil || got.Reference != reference || posts != 1 {
					t.Fatalf("result=%+v err=%v posts=%d", got, err, posts)
				}
			})
		}
	}
}
