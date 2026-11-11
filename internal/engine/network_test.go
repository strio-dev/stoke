package engine

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	cooprwire "github.com/strio-dev/coopr/api"
)

func TestNetworkDefaultRequiresCapabilityAndReachesWire(t *testing.T) {
	for _, supported := range []bool{false, true} {
		posts := 0
		c := rawSocketClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				info := cooprwire.Info{APIVersion: 1, Capabilities: []string{cooprwire.CapabilityExecute}}
				if supported {
					info.Capabilities = append(info.Capabilities, cooprwire.CapabilityRunNetwork)
				}
				_ = json.NewEncoder(w).Encode(info)
				return
			}
			posts++
			var request cooprwire.Request
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Options.Network != "none" {
				t.Error("explicit RUN default was lost", request, err)
			}
			_ = json.NewEncoder(w).Encode(cooprwire.Event{Type: "result", Result: &cooprwire.Result{ImageID: "image"}})
		}))
		_, err := c.Execute(t.Context(), Program{}, Options{Network: "none"})
		if !supported {
			if err == nil || !strings.Contains(err.Error(), "run-network") || posts != 0 {
				t.Fatal("unsupported network reached execution", err, posts)
			}
		} else if err != nil || posts != 1 {
			t.Fatal("network-capable execution failed", err, posts)
		}
	}
}
