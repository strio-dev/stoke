package artifact

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/oras-project/oras-go/v3/registry/remote/credentials"
	"github.com/oras-project/oras-go/v3/registry/remote/properties"
	"oras.land/oras-go/v2/registry"
)

func writeCredentialFixture(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func inlineCredentialFixture(key, username string) string {
	value := base64.StdEncoding.EncodeToString([]byte(username + ":fixture-password"))
	return fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, key, value)
}

func TestRegistryCredentialPrecedence(t *testing.T) {
	root := t.TempDir()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	home, runtimeDir, config, docker := filepath.Join(root, "home"), filepath.Join(root, "runtime"), filepath.Join(root, "config"), filepath.Join(root, "docker")
	registries := filepath.Join(root, "registries.conf")
	writeCredentialFixture(t, registries, "credential-helpers = [\"containers-auth.json\"]\n")
	t.Setenv("CONTAINERS_REGISTRIES_CONF", registries)
	t.Setenv("CONTAINERS_REGISTRIES_CONF_OVERRIDE", "")
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("PATH", root)

	const host, repo = "registry.example.test", "registry.example.test/team/workflow"
	ref, err := registry.ParseReference(repo)
	if err != nil {
		t.Fatal(err)
	}
	explicit := filepath.Join(root, "explicit.json")
	envFile := filepath.Join(root, "environment.json")
	empty := filepath.Join(root, "empty.json")
	writeCredentialFixture(t, explicit, inlineCredentialFixture(repo, "flag"))
	writeCredentialFixture(t, envFile, inlineCredentialFixture("registry.example.test/team", "environment"))
	writeCredentialFixture(t, empty, "{}")
	writeCredentialFixture(t, filepath.Join(docker, "config.json"), inlineCredentialFixture("https://"+host+"/", "docker"))
	writeCredentialFixture(t, filepath.Join(runtimeDir, "containers/auth.json"), inlineCredentialFixture(host, "runtime"))
	writeCredentialFixture(t, filepath.Join(config, "containers/auth.json"), inlineCredentialFixture(repo, "native-repository"))
	t.Setenv("REGISTRY_AUTH_FILE", envFile)
	t.Setenv("DOCKER_CONFIG", docker)

	expect := func(path string, want credentials.Credential) {
		t.Helper()
		lookup, err := registryCredential(path, ref)
		if err != nil {
			t.Fatal(err)
		}
		got, err := lookup(t.Context(), properties.Resource{Registry: host, Path: "team/workflow"})
		if err != nil || got != want {
			t.Fatalf("credential selection for %q did not match fixture: %v", path, err)
		}
	}
	basic := func(username string) credentials.Credential {
		return credentials.Credential{Username: username, Password: "fixture-password"}
	}

	expect(explicit, basic("flag"))
	expect(empty, credentials.EmptyCredential)
	if _, err := registryCredential(filepath.Join(root, "missing.json"), ref); err == nil {
		t.Fatal("missing explicit file was silently accepted")
	}
	expect("", basic("environment"))
	t.Setenv("REGISTRY_AUTH_FILE", empty)
	expect("", credentials.EmptyCredential)
	t.Setenv("REGISTRY_AUTH_FILE", "")
	expect("", basic("docker"))
	t.Setenv("DOCKER_CONFIG", filepath.Join(root, "absent-docker-config"))
	expect("", basic("runtime"))
	t.Setenv("DOCKER_CONFIG", "")
	t.Setenv("XDG_RUNTIME_DIR", root)
	expect("", basic("native-repository"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "absent-native-config"))
	expect("", credentials.EmptyCredential)

	for helper, username := range map[string]string{"global-fixture": "<token>", "host-fixture": "helper"} {
		path := filepath.Join(root, "docker-credential-"+helper)
		writeCredentialFixture(t, path, "#!"+sh+"\n[ \"$1\" = get ] || exit 1\nprintf '%s\\n' '{\"Username\":\""+username+"\",\"Secret\":\"fixture-password\"}'\n")
		if err := os.Chmod(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	helperConfig := `{"credsStore":"global-fixture","credHelpers":{"registry.example.test":"host-fixture"}}`
	writeCredentialFixture(t, explicit, helperConfig)
	expect(explicit, basic("helper"))
	writeCredentialFixture(t, filepath.Join(home, ".docker/config.json"), `{"credsStore":"global-fixture"}`)
	expect("", credentials.Credential{RefreshToken: "fixture-password"})
	writeCredentialFixture(t, filepath.Join(docker, "config.json"),
		strings.TrimSuffix(inlineCredentialFixture(host, "stale-inline"), "}")+`,"credsStore":"global-fixture"}`)
	t.Setenv("DOCKER_CONFIG", docker)
	expect("", credentials.Credential{RefreshToken: "fixture-password"})

	if runtime.GOOS == "linux" {
		// Docker may discover its native helper when its config is empty.
		// An explicitly selected empty auth file stays isolated from it.
		helper := filepath.Join(root, "docker-credential-secretservice")
		writeCredentialFixture(t, helper, "#!"+sh+"\nprintf '%s\\n' '{\"Username\":\"default-helper\",\"Secret\":\"fixture-password\"}'\n")
		if err := os.Chmod(helper, 0700); err != nil {
			t.Fatal(err)
		}
		writeCredentialFixture(t, filepath.Join(docker, "config.json"), "{}")
		expect("", basic("default-helper"))
		expect(empty, credentials.EmptyCredential)
	}
}

func TestSelectedDockerHubCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeCredentialFixture(t, path, `{"auths":{
		"https://index.docker.io/v1/":{"username":"host","password":"fixture"},
		"docker.io/team":{"username":"team","password":"fixture"},
		"docker.io/team/workflow":{"identitytoken":"refresh","registrytoken":"access"},
		"docker.io/team/public":{}
	}}`)
	for repository, want := range map[string]credentials.Credential{
		"team/workflow": {RefreshToken: "refresh", AccessToken: "access"},
		"team/sibling":  {Username: "team", Password: "fixture"},
		"other/sibling": {Username: "host", Password: "fixture"},
		"team/public":   {},
	} {
		ref, err := registry.ParseReference("docker.io/" + repository)
		if err != nil {
			t.Fatal(err)
		}
		lookup, err := registryCredential(path, ref)
		if err != nil {
			t.Fatal(err)
		}
		got, err := lookup(t.Context(), properties.Resource{Registry: "docker.io", Path: repository})
		if err != nil || got != want {
			t.Errorf("unexpected lookup result for %s: %v", repository, err)
		}
	}
}

func TestSelectedCredentialHelperErrorAndMiss(t *testing.T) {
	dir := t.TempDir()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	file := filepath.Join(dir, "config.json")
	helper := filepath.Join(dir, "docker-credential-fixture")
	writeCredentialFixture(t, file, `{"credHelpers":{"registry.example.test":"fixture"},"auths":{"registry.example.test/team/workflow":{"username":"inline","password":"fixture"}}}`)
	ref, err := registry.ParseReference("registry.example.test/team/workflow")
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := registryCredential(file, ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		msg  string
		fail bool
	}{{"fixture helper failed", true}, {"credentials not found in native keychain", false}} {
		writeCredentialFixture(t, helper, "#!"+sh+"\nIFS= read -r key || :\n[ \"$key\" = registry.example.test ] || exit 2\nprintf '%s' '"+tc.msg+"'\nexit 1\n")
		if err := os.Chmod(helper, 0700); err != nil {
			t.Fatal(err)
		}
		got, err := lookup(t.Context(), properties.Resource{Registry: ref.Registry, Path: ref.Repository})
		if (err != nil) != tc.fail || got != credentials.EmptyCredential {
			t.Fatalf("unexpected helper result for %q: %v", tc.msg, err)
		}
	}
	writeCredentialFixture(t, file, `{"auths":`)
	if _, err := registryCredential(file, ref); err == nil {
		t.Fatal("accepted malformed selected file")
	}
}
