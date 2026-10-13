// Package registry finds the credentials to use when pulling an image from a private registry.
//
// Credentials come from a Docker-style config.json (the "auths" section). Credential helpers and
// credential stores are not supported: the updater image contains no helper binaries.
package registry

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	hubHost   = "docker.io"
	hubServer = "https://index.docker.io/v1/"
)

// maxConfigSize caps the size of the config file that is read.
const maxConfigSize = 1 << 20

// RegistryHost returns the registry host of an image reference ("docker.io" for Docker Hub).
// The host is returned as written in the reference (case is kept); matching against config keys
// is case-insensitive.
// The first path component is a host when it contains '.' or ':' or is "localhost".
func RegistryHost(ref string) string {
	first, _, found := strings.Cut(ref, "/")
	if !found {
		return hubHost
	}
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return first
	}
	return hubHost
}

// normalize reduces a config key or registry host to a comparable host: no scheme, no path,
// lower case, and every Docker Hub name folded into "docker.io".
func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	switch s {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return hubHost
	}
	return s
}

type authEntry struct {
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
}

type configFile struct {
	Auths       map[string]authEntry `json:"auths"`
	CredsStore  string               `json:"credsStore"`
	CredHelpers map[string]string    `json:"credHelpers"`
}

// Store looks credentials up in a config.json that is re-read on every call.
type Store struct {
	path string
	warn func(string)

	mu     sync.Mutex
	warned map[string]bool
}

// NewStore returns a store reading dir/config.json. An empty dir disables it (always anonymous).
// warn, which may be nil, receives problems worth reporting, each message at most once; it never
// contains credentials.
func NewStore(dir string, warn func(msg string)) *Store {
	s := &Store{warn: warn, warned: map[string]bool{}}
	if dir != "" {
		s.path = filepath.Join(dir, "config.json")
	}
	return s
}

func (s *Store) warnOnce(msg string) {
	s.mu.Lock()
	seen := s.warned[msg]
	s.warned[msg] = true
	s.mu.Unlock()
	if !seen && s.warn != nil {
		s.warn(msg)
	}
}

func (s *Store) load() (*configFile, bool) {
	f, err := os.Open(s.path)
	var raw []byte
	if err == nil {
		defer f.Close()
		raw, err = io.ReadAll(io.LimitReader(f, maxConfigSize+1))
	}
	if err != nil {
		cause := err.Error()
		var pe *fs.PathError
		if errors.As(err, &pe) {
			cause = pe.Err.Error()
		}
		s.warnOnce("cannot read registry credentials file " + s.path + " (" + cause + "), pulling anonymously")
		return nil, false
	}
	if len(raw) > maxConfigSize {
		s.warnOnce("registry credentials file " + s.path + " is larger than 1 MiB, pulling anonymously")
		return nil, false
	}
	var cfg configFile
	if err := json.Unmarshal(raw, &cfg); err != nil {
		s.warnOnce("registry credentials file " + s.path + " is not valid JSON, pulling anonymously")
		return nil, false
	}
	return &cfg, true
}

func (e authEntry) isEmpty() bool {
	return e == authEntry{}
}

// credentials extracts a usable login from an entry. Like the Docker CLI, "auth" wins over
// "username" and "password"; an empty password is accepted.
func (e authEntry) credentials() (user, pass, token string, ok bool) {
	if e.IdentityToken != "" {
		return "", "", e.IdentityToken, true
	}
	if e.Auth != "" {
		raw, err := base64.StdEncoding.DecodeString(e.Auth)
		if err != nil {
			return "", "", "", false
		}
		u, p, found := strings.Cut(string(raw), ":")
		return u, p, "", found && u != ""
	}
	if e.Username != "" {
		return e.Username, e.Password, "", true
	}
	return "", "", "", false
}

// AuthHeader returns the value of the X-Registry-Auth header for the registry of ref, or "" when
// the image must be pulled anonymously.
func (s *Store) AuthHeader(ref string) string {
	if s == nil || s.path == "" {
		return ""
	}
	cfg, ok := s.load()
	if !ok {
		return ""
	}
	host := RegistryHost(ref)
	want := normalize(host)
	// Deterministic order: the exact key first, then the other keys sorted.
	keys := make([]string, 0, len(cfg.Auths))
	for key := range cfg.Auths {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	sort.SliceStable(keys, func(i, j int) bool { return keys[i] == host && keys[j] != host })
	unusable := false
	for _, key := range keys {
		e := cfg.Auths[key]
		if normalize(key) != want || e.isEmpty() {
			continue
		}
		user, pass, token, ok := e.credentials()
		if !ok {
			unusable = true
			continue
		}
		server := host
		if want == hubHost {
			server = hubServer
		}
		payload := map[string]string{"serveraddress": server}
		if token != "" {
			payload["identitytoken"] = token
		} else {
			payload["username"] = user
			payload["password"] = pass
		}
		b, _ := json.Marshal(payload)
		return base64.URLEncoding.EncodeToString(b)
	}
	if unusable {
		s.warnOnce("registry " + host + " has an entry in the credentials file without usable credentials, pulling anonymously")
		return ""
	}
	helper := cfg.CredsStore != ""
	for key, h := range cfg.CredHelpers {
		if h != "" && normalize(key) == want {
			helper = true
		}
	}
	if helper {
		s.warnOnce("registry " + host + " is configured with a credential helper, which is not supported; add an inline `auth` entry")
	}
	return ""
}
