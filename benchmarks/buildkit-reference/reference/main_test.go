package main

import (
	"fmt"
	"os"
	"syscall"
	"testing"
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
