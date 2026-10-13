package registry

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistryHost(t *testing.T) {
	cases := map[string]string{
		"nginx":                         "docker.io",
		"nginx:latest":                  "docker.io",
		"user/app:1":                    "docker.io",
		"user/app@sha256:abcdef":        "docker.io",
		"ghcr.io/o/i:tag":               "ghcr.io",
		"ghcr.io/o/i":                   "ghcr.io",
		"localhost/x":                   "localhost",
		"localhost:5005/x:1":            "localhost:5005",
		"registry.example.com:5000/a/b": "registry.example.com:5000",
		"registry.example.com:5000/a/b:1@sha256:ab": "registry.example.com:5000",
		"myhost:5000/x":             "myhost:5000",
		"docker.io/library/nginx:1": "docker.io",
	}
	for ref, want := range cases {
		if got := RegistryHost(ref); got != want {
			t.Errorf("RegistryHost(%q) = %q, want %q", ref, got, want)
		}
	}
}

type warnings struct{ msgs []string }

func (w *warnings) add(m string) { w.msgs = append(w.msgs, m) }

func writeConfig(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func decode(t *testing.T, header string) map[string]string {
	t.Helper()
	raw, err := base64.URLEncoding.DecodeString(header)
	if err != nil {
		t.Fatalf("header is not base64url: %v", err)
	}
	out := map[string]string{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAuthHeader(t *testing.T) {
	cfg := `{"auths":{
	  "ghcr.io":{"auth":"` + b64("alice:p:ass") + `"},
	  "https://index.docker.io/v1/":{"auth":"` + b64("bob:hubpass") + `"},
	  "Registry.Example.com:5000":{"username":"carol","password":"explicit"},
	  "http://localhost:5005/some/path":{"auth":"` + b64("dave:local") + `"},
	  "tokens.example.org":{"identitytoken":"tok-123"},
	  "empty.example.org":{}
	}}`
	cases := []struct {
		name, ref string
		want      map[string]string
	}{
		{"auth field, password with colon", "ghcr.io/o/i:1", map[string]string{"username": "alice", "password": "p:ass", "serveraddress": "ghcr.io"}},
		{"docker hub library image", "nginx:latest", map[string]string{"username": "bob", "password": "hubpass", "serveraddress": "https://index.docker.io/v1/"}},
		{"docker hub user image", "user/app:1", map[string]string{"username": "bob", "password": "hubpass", "serveraddress": "https://index.docker.io/v1/"}},
		{"docker.io explicit host", "docker.io/library/nginx", map[string]string{"username": "bob", "password": "hubpass", "serveraddress": "https://index.docker.io/v1/"}},
		{"registry-1 alias", "registry-1.docker.io/library/nginx", map[string]string{"username": "bob", "password": "hubpass", "serveraddress": "https://index.docker.io/v1/"}},
		{"explicit username/password, case-insensitive host", "registry.example.com:5000/a/b", map[string]string{"username": "carol", "password": "explicit", "serveraddress": "registry.example.com:5000"}},
		{"scheme and path stripped", "localhost:5005/x:1", map[string]string{"username": "dave", "password": "local", "serveraddress": "localhost:5005"}},
		{"identity token", "tokens.example.org/x", map[string]string{"identitytoken": "tok-123", "serveraddress": "tokens.example.org"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeConfig(t, dir, cfg)
			h := NewStore(dir, nil).AuthHeader(c.ref)
			got := decode(t, h)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
		})
	}
}

func TestAuthHeaderAnonymous(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"auths":{"ghcr.io":{"auth":"`+b64("a:b")+`"},"empty.example.org":{},"bad.example.org":{"auth":"!!notbase64"}}}`)
	w := &warnings{}
	s := NewStore(dir, w.add)
	for _, ref := range []string{"quay.io/x/y", "nginx", "empty.example.org/a", "bad.example.org/a"} {
		if h := s.AuthHeader(ref); h != "" {
			t.Errorf("%s: expected anonymous, got %q", ref, h)
		}
	}
	for _, m := range w.msgs {
		if strings.Contains(m, "notbase64") {
			t.Errorf("warning leaks an auth value: %s", m)
		}
	}
}

func TestDisabledStore(t *testing.T) {
	w := &warnings{}
	if h := NewStore("", w.add).AuthHeader("ghcr.io/o/i"); h != "" || len(w.msgs) != 0 {
		t.Fatalf("%q %v", h, w.msgs)
	}
	var nilStore *Store
	if nilStore.AuthHeader("x") != "" {
		t.Fatal("nil store must be anonymous")
	}
}

func TestMissingFileWarnsOnce(t *testing.T) {
	w := &warnings{}
	s := NewStore(t.TempDir(), w.add)
	for i := 0; i < 3; i++ {
		if s.AuthHeader("ghcr.io/o/i") != "" {
			t.Fatal("expected anonymous")
		}
	}
	if len(w.msgs) != 1 {
		t.Fatalf("expected one warning, got %v", w.msgs)
	}
}

func TestInvalidJSONWarnsOnceAndNeverLeaks(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"auths":{"ghcr.io":{"auth":"SUPERSECRET"`)
	w := &warnings{}
	s := NewStore(dir, w.add)
	for i := 0; i < 3; i++ {
		if s.AuthHeader("ghcr.io/o/i") != "" {
			t.Fatal("expected anonymous")
		}
	}
	if len(w.msgs) != 1 {
		t.Fatalf("expected one warning, got %v", w.msgs)
	}
	if strings.Contains(w.msgs[0], "SUPERSECRET") {
		t.Fatalf("warning leaks a secret: %s", w.msgs[0])
	}
	// A different error is warned about again.
	os.Remove(filepath.Join(dir, "config.json"))
	s.AuthHeader("ghcr.io/o/i")
	if len(w.msgs) != 2 {
		t.Fatalf("expected a new warning for a new error, got %v", w.msgs)
	}
}

func TestCredentialHelperWarnsOncePerRegistry(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"auths":{"ghcr.io":{}},"credsStore":"osxkeychain","credHelpers":{"quay.io":"ecr-login"}}`)
	w := &warnings{}
	s := NewStore(dir, w.add)
	for i := 0; i < 3; i++ {
		for _, ref := range []string{"ghcr.io/o/i", "quay.io/o/i", "nginx"} {
			if s.AuthHeader(ref) != "" {
				t.Fatal("expected anonymous")
			}
		}
	}
	if len(w.msgs) != 3 {
		t.Fatalf("expected one warning per registry, got %v", w.msgs)
	}
	if !strings.Contains(w.msgs[0], "ghcr.io") || !strings.Contains(w.msgs[0], "credential helper") {
		t.Fatalf("unexpected warning: %s", w.msgs[0])
	}
}

func TestInlineAuthWinsOverCredentialHelper(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"auths":{"ghcr.io":{"auth":"`+b64("u:p")+`"}},"credsStore":"desktop"}`)
	w := &warnings{}
	if NewStore(dir, w.add).AuthHeader("ghcr.io/o/i") == "" || len(w.msgs) != 0 {
		t.Fatalf("inline auth must be used silently: %v", w.msgs)
	}
}

func TestFileIsReReadOnEveryCall(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir, nil)
	writeConfig(t, dir, `{"auths":{"ghcr.io":{"auth":"`+b64("u:one")+`"}}}`)
	if got := decode(t, s.AuthHeader("ghcr.io/o/i"))["password"]; got != "one" {
		t.Fatalf("got %q", got)
	}
	writeConfig(t, dir, `{"auths":{"ghcr.io":{"auth":"`+b64("u:two")+`"}}}`)
	if got := decode(t, s.AuthHeader("ghcr.io/o/i"))["password"]; got != "two" {
		t.Fatalf("rotated token not picked up: %q", got)
	}
}

func TestWarningsNeverContainSecrets(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"auths":{"ghcr.io":{"auth":"`+b64("user:TOPSECRET")+`","password":"TOPSECRET"}},"credHelpers":{"quay.io":"x"}}`)
	w := &warnings{}
	s := NewStore(dir, w.add)
	s.AuthHeader("ghcr.io/o/i")
	s.AuthHeader("quay.io/o/i")
	for _, m := range w.msgs {
		if strings.Contains(m, "TOPSECRET") || strings.Contains(m, b64("user:TOPSECRET")) {
			t.Fatalf("warning leaks a secret: %s", m)
		}
	}
}
