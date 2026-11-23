package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/strio-dev/stoke/internal/runner"
)

var hostCapabilityName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]{0,127}$`)

// Host authority comes only from caller-supplied execution flags. Workflow and
// plan declarations describe requirements but never grant runner authority.
func hostExecutionFlags(fs *flag.FlagSet, options *runner.Options) {
	fs.BoolVar(&options.AllowHost, "allow-host", false, "explicitly trust selected host tasks to execute with the runner user's authority")
	add := func(values []string) error {
		if len(options.HostCapabilities)+len(values) > 256 {
			return errors.New("at most 256 host capabilities may be granted")
		}
		seen := slices.Clone(options.HostCapabilities)
		for _, value := range values {
			if !hostCapabilityName.MatchString(value) || slices.Contains(seen, value) {
				return fmt.Errorf("host capability %q must be a distinct bounded capability name", value)
			}
			seen = append(seen, value)
		}
		options.HostCapabilities = seen
		return nil
	}
	fs.Func("host-capability", "explicitly attest a provisioned host capability (repeatable; installs or enables nothing)", func(value string) error {
		return add([]string{value})
	})
	fs.Func("host-capabilities-json", "JSON array of explicitly attested provisioned host capabilities (for CI input transport)", func(raw string) error {
		var values []string
		if err := decodeHostJSON(raw, &values, 16<<10); err != nil {
			return fmt.Errorf("host capabilities require one JSON string array: %w", err)
		}
		if values == nil {
			return errors.New("host capabilities require a JSON array, not null")
		}
		return add(values)
	})
}

func decodeHostJSON(raw string, value any, limit int) error {
	if len(raw) > limit {
		return errors.New("CI metadata exceeds its size limit")
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("CI metadata requires one JSON document")
	}
	return nil
}
