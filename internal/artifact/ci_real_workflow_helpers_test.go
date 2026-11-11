package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

type realCIUser struct {
	Name   string   `json:"name"`
	UID    uint32   `json:"uid"`
	GID    uint32   `json:"gid"`
	Groups []uint32 `json:"groups"`
}

type realCIExecution struct {
	users       map[string]realCIUser
	driver      string
	network     string
	pruneNative bool
	temporary   string
	mu          sync.Mutex
	identities  map[string]realCIUser
}

// This opt-in test coordinator can run as root inside a disposable VM while
// every runtime CLI uses an explicitly selected unprivileged guest identity.
// Ordinary fixture invocations retain their existing user and command behavior.
func (f *ciRegistryFixture) configureRealCIExecution() *realCIExecution {
	f.t.Helper()
	config := &realCIExecution{driver: os.Getenv("STOKE_REAL_CI_STORAGE_DRIVER"), network: os.Getenv("STOKE_REAL_CI_NETWORK"), temporary: os.Getenv("STOKE_REAL_CI_SCRATCH_TMP"), identities: map[string]realCIUser{}}
	if config.driver != "" && config.driver != "overlay" && config.driver != "vfs" {
		f.t.Fatal("STOKE_REAL_CI_STORAGE_DRIVER must be overlay or vfs")
	}
	if raw := os.Getenv("STOKE_REAL_CI_PRUNE_NATIVE"); raw != "" {
		if raw != "1" || config.driver == "" || os.Geteuid() == 0 || os.Getenv("STOKE_REAL_CI_USERS") != "" {
			f.t.Fatal("STOKE_REAL_CI_PRUNE_NATIVE accepts only 1 with an explicit storage driver and a non-root single-user coordinator")
		}
		config.pruneNative = true
	}
	if raw := os.Getenv("STOKE_REAL_CI_USERS"); raw != "" {
		if os.Geteuid() != 0 {
			f.t.Fatal("STOKE_REAL_CI_USERS requires a disposable guest root coordinator")
		}
		var names map[string]string
		if err := json.Unmarshal([]byte(raw), &names); err != nil || len(names) != 3 {
			f.t.Fatal("STOKE_REAL_CI_USERS must name local, ci and return users")
		}
		config.users = map[string]realCIUser{}
		seen := map[uint32]bool{}
		for _, role := range []string{"local", "ci", "return"} {
			u, err := user.Lookup(names[role])
			if err != nil {
				f.t.Fatalf("guest user %s: %v", role, err)
			}
			uid, err := strconv.ParseUint(u.Uid, 10, 32)
			if err != nil || uid == 0 || seen[uint32(uid)] {
				f.t.Fatal("guest runtime users must have distinct non-root UIDs")
			}
			gid, err := strconv.ParseUint(u.Gid, 10, 32)
			if err != nil {
				f.t.Fatal(err)
			}
			groups, err := u.GroupIds()
			if err != nil {
				f.t.Fatal(err)
			}
			selected := realCIUser{Name: u.Username, UID: uint32(uid), GID: uint32(gid)}
			for _, value := range groups {
				n, err := strconv.ParseUint(value, 10, 32)
				if err != nil {
					f.t.Fatal(err)
				}
				selected.Groups = append(selected.Groups, uint32(n))
			}
			seen[selected.UID] = true
			config.users[role] = selected
		}
		if config.temporary == "" || !filepath.IsAbs(config.temporary) {
			f.t.Fatal("multi-user dogfood requires an absolute STOKE_REAL_CI_SCRATCH_TMP")
		}
		if err := os.Chmod(f.work, 0711); err != nil {
			f.t.Fatal(err)
		}
		if err := os.Chmod(f.policy, 0644); err != nil {
			f.t.Fatal(err)
		}
	}
	if config.driver != "" || config.users != nil {
		f.prepareCommand = func(label, caller string, command *exec.Cmd) {
			args := command.Args[1:]
			runtimeCall := len(args) > 0 && args[0] == "run" || len(args) > 1 && args[0] == "ci" && args[1] == "run"
			if !runtimeCall {
				return
			}
			if config.driver != "" {
				position := 2
				if args[0] == "ci" {
					position = 3
				}
				selected := append([]string{}, command.Args[:position]...)
				selected = append(selected, "--storage-driver", config.driver)
				command.Args = append(selected, command.Args[position:]...)
			}
			if config.users == nil {
				return
			}
			role := "local"
			if strings.HasPrefix(label, "real-provider-") || strings.HasPrefix(label, "real-fresh-") {
				role = "ci"
			}
			if strings.HasPrefix(label, "real-local-return") {
				role = "return"
			}
			selected := config.users[role]
			private := filepath.Join(f.work, label)
			if err := filepath.WalkDir(private, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				return os.Lchown(p, int(selected.UID), int(selected.GID))
			}); err != nil {
				f.t.Fatal(err)
			}
			temporary := filepath.Join(config.temporary, strconv.FormatUint(uint64(selected.UID), 10))
			if err := os.MkdirAll(temporary, 0700); err != nil {
				f.t.Fatal(err)
			}
			if err := os.Chown(temporary, int(selected.UID), int(selected.GID)); err != nil {
				f.t.Fatal(err)
			}
			for i, arg := range command.Args {
				if arg == "--github-output" && i+1 < len(command.Args) {
					if err := os.Chown(command.Args[i+1], int(selected.UID), int(selected.GID)); err != nil {
						f.t.Fatal(err)
					}
				}
			}
			command.Dir = caller
			command.Env = append(command.Env, "USER="+selected.Name, "LOGNAME="+selected.Name, "XDG_RUNTIME_DIR=/run/user/"+strconv.FormatUint(uint64(selected.UID), 10), "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/"+strconv.FormatUint(uint64(selected.UID), 10)+"/bus", "TMPDIR="+temporary)
			command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: selected.UID, Gid: selected.GID, Groups: selected.Groups}}
			probe := exec.CommandContext(f.ctx, "id", "-u")
			probe.Env = command.Env
			probe.SysProcAttr = command.SysProcAttr
			probe.Dir = caller
			output, err := probe.Output()
			if err != nil || strings.TrimSpace(string(output)) != strconv.FormatUint(uint64(selected.UID), 10) {
				f.t.Fatalf("guest credential probe %s: %v %s", label, err, output)
			}
			config.mu.Lock()
			defer config.mu.Unlock()
			config.identities[label] = selected
			f.write(filepath.Join(f.work, "execution-identities.json"), f.encode(config.identities), 0600)
		}
	}
	f.write(filepath.Join(f.work, "execution-configuration.json"), f.encode(map[string]any{"users": config.users, "storageDriver": config.driver, "network": config.network, "pruneNative": config.pruneNative, "temporaryRoot": config.temporary, "coordinatorUID": os.Geteuid()}), 0600)
	return config
}

// Only completed, asserted phases can retire this invocation's disposable native
// images/cache mounts. Exported results and the registry are outside these stores.
func (f *ciRegistryFixture) pruneRealNative(label string, report ciRegistryReport, execution *realCIExecution) {
	f.t.Helper()
	if !execution.pruneNative {
		return
	}
	work, err := filepath.EvalSymlinks(f.work)
	if err != nil {
		f.t.Fatal(err)
	}
	directory, err := filepath.EvalSymlinks(report.Directory)
	if err != nil {
		f.t.Fatal(err)
	}
	relative, err := filepath.Rel(work, directory)
	if err != nil || !filepath.IsAbs(report.Directory) || relative == "." || !filepath.IsLocal(relative) || report.Status != "passed" {
		f.t.Fatal("native prune requires a successful report strictly inside this invocation's work directory")
	}
	for _, name := range []string{"graphroot", "runroot"} {
		selected := filepath.Join(directory, name)
		resolved, err := filepath.EvalSymlinks(selected)
		if err != nil || resolved != selected {
			f.t.Fatalf("native prune store must exist without symlink escapes: %s: %v", selected, err)
		}
		info, err := os.Stat(selected)
		if err != nil || !info.IsDir() {
			f.t.Fatalf("native prune store is not a directory: %s: %v", selected, err)
		}
	}
	args := []string{"--root", filepath.Join(directory, "graphroot"), "--runroot", filepath.Join(directory, "runroot"), "--storage-driver", execution.driver, "image", "prune", "--all", "--force"}
	command := exec.CommandContext(f.ctx, f.engine, args...)
	storageConfig := filepath.Join(f.work, label+".native-prune.storage.conf")
	f.write(storageConfig, []byte(fmt.Sprintf("[storage]\ndriver = %q\ngraphroot = %q\nrunroot = %q\nimagestore = \"\"\n", execution.driver, filepath.Join(directory, "graphroot"), filepath.Join(directory, "runroot"))), 0600)
	// The explicit config excludes system/user files and drop-ins; _OVERRIDE is
	// independently honored upstream and must also be cleared for this command.
	command.Env = append(f.commandFor(label, f.work, nil).Env, "CONTAINERS_STORAGE_CONF="+storageConfig, "CONTAINERS_STORAGE_CONF_OVERRIDE=", "STORAGE_OPTS=")
	output, err := command.CombinedOutput()
	f.write(filepath.Join(f.work, label+".native-prune.log"), output, 0600)
	f.write(filepath.Join(f.work, label+".native-prune.json"), f.encode(map[string]any{"command": command.Args, "directory": directory, "storageDriver": execution.driver, "storageConfig": storageConfig, "succeeded": err == nil, "error": fmt.Sprint(err)}), 0600)
	if err != nil {
		f.t.Fatalf("scoped native prune %s: %v: %s", label, err, output)
	}
}

func (f *ciRegistryFixture) copyRealCheckout(source, destination string, shared, fresh bool) {
	f.t.Helper()
	if !filepath.IsAbs(destination) || filepath.Dir(destination) != f.work {
		f.t.Fatal("dogfood checkout must be a direct owned fixture child")
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		f.t.Fatal("dogfood checkout destination already exists")
	}
	copy := exec.CommandContext(f.ctx, "cp", "-a", "--", source+string(os.PathSeparator)+".", destination)
	if output, err := copy.CombinedOutput(); err != nil {
		f.t.Fatalf("complete source snapshot: %v\n%s", err, output)
	}
	type entry struct {
		Path, Kind, SHA256, Link string
		Mode                     fs.FileMode
		UID, GID                 uint32
		Mtime                    int64
	}
	entries := []entry{}
	timestamp := time.Now().Add(3 * time.Second)
	err := filepath.WalkDir(destination, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(destination, p)
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		record := entry{Path: filepath.ToSlash(rel), Mode: info.Mode().Perm(), Mtime: info.ModTime().UnixNano()}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			record.UID, record.GID = st.Uid, st.Gid
		}
		switch {
		case d.IsDir():
			record.Kind = "directory"
		case info.Mode().IsRegular():
			record.Kind = "file"
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			record.SHA256 = hex.EncodeToString(sum[:])
			original, err := os.ReadFile(filepath.Join(source, rel))
			if err != nil {
				return err
			}
			if !bytes.Equal(original, data) {
				return fmt.Errorf("complete checkout changed %s", rel)
			}
		case info.Mode()&os.ModeSymlink != 0:
			record.Kind = "symlink"
			record.Link, err = os.Readlink(p)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported complete-checkout member %s", rel)
		}
		entries = append(entries, record)
		if shared {
			if err := os.Lchown(p, 0, 0); err != nil {
				return err
			}
			if record.Kind != "symlink" {
				mode := info.Mode().Perm() | 0444
				if d.IsDir() {
					mode |= 0111
				}
				mode &^= 0222
				if err := os.Chmod(p, mode); err != nil {
					return err
				}
			}
		}
		if fresh && record.Kind != "symlink" {
			if err := os.Chtimes(p, timestamp, timestamp); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.write(filepath.Join(f.work, filepath.Base(destination)+"-source-manifest.json"), f.encode(map[string]any{"source": source, "destination": destination, "completeCopy": true, "sourceEntriesBeforeAccessNormalization": entries, "sharedReadOnlyRootOwned": shared, "freshTimestampProbe": fresh, "freshTimestamp": timestamp}), 0600)
}

func TestRealDriverSelectionPrecedesTargetsAndPreservesGeneratedArguments(t *testing.T) {
	t.Setenv("STOKE_REAL_CI_USERS", "")
	t.Setenv("STOKE_REAL_CI_STORAGE_DRIVER", "overlay")
	work := t.TempDir()
	f := &ciRegistryFixture{t: t, ctx: t.Context(), work: work, binary: "/bin/true"}
	f.configureRealCIExecution()
	ordinary := f.command("real-local-producer", work, filepath.Join(work, "workflow.yaml"), []string{"build"})
	if ordinary.Args[1] != "run" || ordinary.Args[2] != "--storage-driver" || ordinary.Args[3] != "overlay" || ordinary.Args[len(ordinary.Args)-1] != "build" || ordinary.Dir != "" {
		t.Fatalf("runtime driver selection changed command semantics: %#v", ordinary)
	}
	generated := f.generatedCommand("real-provider-build", work, []string{"ci", "run", "--plan", "plan.json", "--job", "build"})
	if strings.Join(generated.Args[1:], " ") != "ci run --storage-driver overlay --plan plan.json --job build" || generated.Dir != work {
		t.Fatalf("generated command discarded runtime setup: %#v", generated)
	}
	plan := f.generatedCommand("real-ci-generate", work, []string{"ci", "generate", "--context", "."})
	if strings.Contains(strings.Join(plan.Args, " "), "--storage-driver") {
		t.Fatal("runtime-only driver option escaped into generation")
	}
}

func realFileSHA256(filename string) (string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func realImageFileSHA256(directory, filename, expectedManifest string) (string, error) {
	indexData, err := os.ReadFile(filepath.Join(directory, "index.json"))
	if err != nil {
		return "", err
	}
	var index v1.Index
	if err = json.Unmarshal(indexData, &index); err != nil {
		return "", err
	}
	if len(index.Manifests) != 1 {
		return "", fmt.Errorf("expected one canonical root")
	}
	root := index.Manifests[0]
	if root.Digest.String() != expectedManifest {
		return "", fmt.Errorf("canonical image manifest mismatch")
	}
	blob := func(digest string) ([]byte, error) {
		parts := strings.Split(digest, ":")
		if len(parts) != 2 || parts[0] != "sha256" || len(parts[1]) != 64 {
			return nil, fmt.Errorf("unsupported image blob digest")
		}
		p := filepath.Join(directory, "blobs", parts[0], parts[1])
		file, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		defer func() { _ = file.Close() }()
		data, err := io.ReadAll(io.LimitReader(file, 256<<20+1))
		if err != nil {
			return nil, err
		}
		if len(data) > 256<<20 {
			return nil, fmt.Errorf("binary proof layer exceeds limit")
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != parts[1] {
			return nil, fmt.Errorf("image blob integrity mismatch")
		}
		return data, nil
	}
	rootData, err := blob(root.Digest.String())
	if err != nil {
		return "", err
	}
	var manifest v1.Manifest
	if err = json.Unmarshal(rootData, &manifest); err != nil {
		return "", err
	}
	selected := ""
	for _, layer := range manifest.Layers {
		data, err := blob(layer.Digest.String())
		if err != nil {
			return "", err
		}
		var reader io.Reader = bytes.NewReader(data)
		if strings.HasSuffix(layer.MediaType, "+gzip") || strings.HasSuffix(layer.MediaType, ".gzip") {
			gzipReader, err := gzip.NewReader(reader)
			if err != nil {
				return "", err
			}
			defer func() { _ = gzipReader.Close() }()
			reader = gzipReader
		} else if layer.MediaType != v1.MediaTypeImageLayer && layer.MediaType != "application/vnd.docker.image.rootfs.diff.tar" {
			return "", fmt.Errorf("unsupported binary proof layer %s", layer.MediaType)
		}
		tarReader := tar.NewReader(io.LimitReader(reader, (512<<20)+1))
		for {
			header, err := tarReader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", err
			}
			name := strings.TrimPrefix(header.Name, "./")
			if path.Clean(name) != name || strings.HasPrefix(name, "../") {
				return "", fmt.Errorf("unsafe image path")
			}
			if strings.TrimPrefix(name, "/") != strings.TrimPrefix(filename, "/") {
				continue
			}
			if header.Typeflag != tar.TypeReg && header.Typeflag != 0 || header.Size > 128<<20 {
				return "", fmt.Errorf("binary proof target is not a bounded file")
			}
			hash := sha256.New()
			if _, err := io.Copy(hash, tarReader); err != nil {
				return "", err
			}
			selected = hex.EncodeToString(hash.Sum(nil))
		}
	}
	if selected == "" {
		return "", fmt.Errorf("binary proof target not present")
	}
	return selected, nil
}

func TestRealImageBinaryProofVerifiesManifestAndLayer(t *testing.T) {
	directory := t.TempDir()
	payload := []byte("actual compiled output")
	var layer bytes.Buffer
	writer := tar.NewWriter(&layer)
	if err := writer.WriteHeader(&tar.Header{Name: "stoke", Mode: 0755, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	put := func(data []byte) string {
		sum := sha256.Sum256(data)
		value := hex.EncodeToString(sum[:])
		p := filepath.Join(directory, "blobs", "sha256", value)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0644); err != nil {
			t.Fatal(err)
		}
		return "sha256:" + value
	}
	layerDigest := put(layer.Bytes())
	manifestData, err := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": v1.MediaTypeImageManifest, "layers": []any{map[string]any{"mediaType": v1.MediaTypeImageLayer, "digest": layerDigest, "size": layer.Len()}}})
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := put(manifestData)
	indexData, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": []any{map[string]any{"mediaType": v1.MediaTypeImageManifest, "digest": manifestDigest, "size": len(manifestData)}}})
	if err := os.WriteFile(filepath.Join(directory, "index.json"), indexData, 0644); err != nil {
		t.Fatal(err)
	}
	expected := sha256.Sum256(payload)
	got, err := realImageFileSHA256(directory, "/stoke", manifestDigest)
	if err != nil || got != hex.EncodeToString(expected[:]) {
		t.Fatalf("binary proof: %s %v", got, err)
	}
	if _, err := realImageFileSHA256(directory, "/stoke", "sha256:"+strings.Repeat("0", 64)); err == nil {
		t.Fatal("wrong root accepted")
	}
	if err := os.WriteFile(filepath.Join(directory, "blobs", "sha256", strings.TrimPrefix(layerDigest, "sha256:")), []byte("tampered"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := realImageFileSHA256(directory, "/stoke", manifestDigest); err == nil {
		t.Fatal("corrupt layer accepted")
	}
}
