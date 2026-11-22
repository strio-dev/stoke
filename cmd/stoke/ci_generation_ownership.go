package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/strio-dev/stoke/internal/ci"
)

const stageChecksumDirectory = ci.StageDirectory + "/.checksums"

// Origin is the workflow path and logical template source, not the rendered
// outputs, template contents or current selection. Partial runs can therefore
// contribute outputs to the same origin without making sibling stages stale.
func stageGenerationScope(workflow, selector, templateFile string) (string, error) {
	source := "selector:" + selector
	if templateFile != "" {
		if filepath.IsAbs(templateFile) {
			cwd, err := os.Getwd()
			if err != nil {
				return "", err
			}
			templateFile, err = filepath.Rel(cwd, templateFile)
			if err != nil {
				return "", err
			}
		}
		source = "file:" + filepath.ToSlash(filepath.Clean(templateFile))
	}
	return stageDigest([]byte(filepath.ToSlash(filepath.Clean(workflow)) + "\x00" + source)), nil
}

func stageChecksumPath(body string) string {
	return filepath.Join(stageChecksumDirectory, strings.TrimPrefix(body, ci.StageDirectory+"/")+".sha256")
}

func stageDigest(body []byte) string {
	return strings.TrimSuffix(string(ci.StageChecksum(body)), "\n")
}

func validStageDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

type stageStamp struct {
	Hash  string `json:"sha256"`
	Scope string `json:"scope,omitempty"`
}

type stageOwnership struct {
	Version int        `json:"version"`
	Current stageStamp `json:"current,omitzero"`
	Pending stageStamp `json:"pending,omitzero"`
}

func parseStageOwnership(data []byte) (stageOwnership, bool) {
	// Existing SHA-256 sidecars have integrity information but no origin.
	if hash, found := strings.CutSuffix(string(data), "\n"); found && validStageDigest(hash) {
		return stageOwnership{Current: stageStamp{Hash: hash}}, true
	}
	var owner stageOwnership
	if err := json.Unmarshal(data, &owner); err != nil || owner.Version != 1 ||
		(owner.Current == (stageStamp{}) && owner.Pending == (stageStamp{})) {
		return stageOwnership{}, false
	}
	validCurrent := owner.Current == (stageStamp{}) || (validStageDigest(owner.Current.Hash) &&
		(owner.Current.Scope == "" || validStageDigest(owner.Current.Scope)))
	validPending := owner.Pending == (stageStamp{}) || (validStageDigest(owner.Pending.Hash) &&
		validStageDigest(owner.Pending.Scope))
	return owner, validCurrent && validPending
}

func (o stageOwnership) match(body []byte) stageStamp {
	hash := stageDigest(body)
	if o.Pending.Hash == hash {
		return o.Pending
	}
	if o.Current.Hash == hash {
		return o.Current
	}
	return stageStamp{}
}

type stageUpdate struct {
	path, checksum string
	data           []byte
	previous, next stageStamp
	replaceBody    bool
}

func prepareStageUpdate(stage ci.StageFile, scope string, check bool) (stageUpdate, error) {
	path, checksum := stage.Path, stageChecksumPath(stage.Path)
	for _, name := range []string{path, checksum} {
		if err := safeGeneratedPath(name); err != nil {
			return stageUpdate{}, err
		}
	}
	var owner stageOwnership
	var previous stageStamp
	metadata, err := os.ReadFile(checksum)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// Do not read the body of an unmanaged collision.
		if _, err := os.Lstat(path); err == nil {
			return stageUpdate{}, fmt.Errorf("refusing to overwrite unmanaged stage %s", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return stageUpdate{}, err
		}
	case err != nil:
		return stageUpdate{}, err
	default:
		var valid bool
		owner, valid = parseStageOwnership(metadata)
		if !valid {
			return stageUpdate{}, fmt.Errorf("invalid ownership checksum for generated stage %s", path)
		}
		body, err := os.ReadFile(path)
		if err == nil {
			previous = owner.match(body)
			if previous == (stageStamp{}) {
				return stageUpdate{}, fmt.Errorf("refusing to overwrite edited stage %s", path)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return stageUpdate{}, err
		}
	}

	next := stageStamp{Hash: stageDigest(stage.Data), Scope: scope}
	replaceBody := previous.Hash != next.Hash
	if check {
		if replaceBody || owner.Pending != (stageStamp{}) {
			return stageUpdate{}, fmt.Errorf("generated stage is missing or stale: %s; regenerate and review", path)
		}
		return stageUpdate{}, nil
	}
	if !replaceBody && previous.Scope != "" {
		// Identical bytes do not move ownership to a different origin.
		next = previous
		if owner.Pending == (stageStamp{}) {
			return stageUpdate{}, nil
		}
	}
	return stageUpdate{path, checksum, stage.Data, previous, next, replaceBody}, nil
}

func (u stageUpdate) apply(write func(string, []byte) error) error {
	store := func(current, pending stageStamp) error {
		data, err := json.Marshal(stageOwnership{Version: 1, Current: current, Pending: pending})
		if err != nil {
			return err
		}
		return write(u.checksum, append(data, '\n'))
	}
	if u.replaceBody {
		// Checksum intent must be on disk before the body rename. If the
		// process stops, neither arbitrary bytes nor a later render can
		// authorize recovery: only the recorded prior and next hashes can.
		if err := store(u.previous, u.next); err != nil {
			return err
		}
		if err := write(u.path, u.data); err != nil {
			return err
		}
	}
	return store(u.next, stageStamp{})
}

func checkObsoleteStages(files []ci.StageFile, scope string) error {
	// WalkDir does not follow symlinks below its root; verify all parents
	// of the root separately so it cannot follow one to get there.
	for _, dir := range []string{filepath.Dir(ci.StageDirectory), ci.StageDirectory, stageChecksumDirectory} {
		info, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("generated checksum directory %q traverses a symlink or nondirectory", dir)
		}
	}

	expected := make(map[string]bool, len(files))
	for _, file := range files {
		expected[stageChecksumPath(file.Path)] = true
	}
	var obsolete []string
	err := filepath.WalkDir(stageChecksumDirectory, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(name, ".sha256") || expected[name] {
			return nil
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		owner, valid := parseStageOwnership(data)
		if !valid || (owner.Current.Scope != scope && owner.Pending.Scope != scope) {
			return nil
		}
		path := ci.StageDirectory + "/" + strings.TrimSuffix(strings.TrimPrefix(filepath.ToSlash(name), stageChecksumDirectory+"/"), ".sha256")
		// If different origins are involved in a pending move, a matching
		// body disambiguates which one owns it. Otherwise report the drift
		// conservatively without modifying the body or sidecar.
		if owner.Current.Hash != "" && owner.Pending.Hash != "" && owner.Current.Scope != owner.Pending.Scope {
			if err := safeGeneratedPath(path); err == nil {
				if body, err := os.ReadFile(path); err == nil {
					if current := owner.match(body); current != (stageStamp{}) && current.Scope != scope {
						return nil
					}
				}
			}
		}
		obsolete = append(obsolete, path)
		return nil
	})
	if err != nil {
		return err
	}
	if len(obsolete) != 0 {
		return fmt.Errorf("obsolete managed stage files: %s; review the files and their checksums before regenerating", strings.Join(obsolete, ", "))
	}
	return nil
}
