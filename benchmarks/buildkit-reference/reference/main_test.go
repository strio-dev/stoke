package main

import (
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestNetworkNamespaceReceipt(t *testing.T) {
	own, err := os.Stat("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	id := own.Sys().(*syscall.Stat_t)
	same := fmt.Sprintf("%d:%d", uint64(id.Dev), id.Ino)
	other := fmt.Sprintf("%d:%d", uint64(id.Dev), id.Ino+1)
	for _, test := range []struct {
		name, receipt string
		wantError     bool
	}{
		{"same", same, true},
		{"different", other, false},
		{"missing", "", true},
		{"malformed", "not:a-namespace", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := requireDifferentNetworkNamespace(test.receipt); (err != nil) != test.wantError {
				t.Fatalf("namespace receipt: %v", err)
			}
		})
	}
}

func TestScratchImageHistory(t *testing.T) {
	config := scratchImageConfig(v1.Platform{OS: "linux", Architecture: "amd64"})
	if config.Created == nil || !config.Created.Equal(time.Unix(0, 0)) || len(config.History) != 1 {
		t.Fatalf("one reproducible scratch filesystem layer required: %+v", config)
	}
	history := config.History[0]
	if history.EmptyLayer || history.Created == nil || !history.Created.Equal(*config.Created) || history.CreatedBy != "COPY /stoke /stoke" {
		t.Fatalf("authored history must survive remote cache restoration: %+v", history)
	}
}
