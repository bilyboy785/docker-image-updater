package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTest(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewWithBase(srv.URL, srv.Client())
}

func TestListContainers(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/containers/json" {
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
		w.Write([]byte(`[{"Id":"abc","Names":["/web"],"Image":"nginx:latest","ImageID":"sha256:111","Labels":{"a":"b"},"HostConfig":{"NetworkMode":"container:xyz"}}]`))
	})
	got, err := c.ListContainers(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	if got[0].Name() != "web" || got[0].ImageID != "sha256:111" || got[0].Labels["a"] != "b" || got[0].NetworkMode != "container:xyz" {
		t.Fatalf("%+v", got[0])
	}
}

func TestInspectContainer(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Id":"abc123","Name":"/web","Config":{"Image":"nginx:latest"},"HostConfig":{"NetworkMode":"bridge"},
		"Mounts":[{"Type":"volume","Name":"v","Destination":"/data","RW":true}],
		"NetworkSettings":{"Networks":{"bridge":{"Aliases":null}}}}`))
	})
	info, err := c.InspectContainer(context.Background(), "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if info.Config["Image"] != "nginx:latest" || info.Mounts[0].Name != "v" || info.NetworkSettings.Networks["bridge"] == nil {
		t.Fatalf("%+v", info)
	}
}

func TestPullImage(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/images/create" || r.URL.Query().Get("fromImage") != "nginx" || r.URL.Query().Get("tag") != "latest" {
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
		w.Write([]byte(`{"status":"Pulling"}` + "\n" + `{"status":"Done"}` + "\n"))
	})
	if err := c.PullImage(context.Background(), "nginx:latest", ""); err != nil {
		t.Fatal(err)
	}
}

// The Engine API pulls every tag of a repository when no tag is given, so PullImage must always
// send an explicit tag (or digest) and a fromImage without one.
func TestPullImageAlwaysSendsOneTag(t *testing.T) {
	for _, tc := range []struct{ ref, from, tag string }{
		{"nginx", "nginx", "latest"},
		{"nginx:1.25", "nginx", "1.25"},
		{"ghcr.io/bakito/adguardhome-sync", "ghcr.io/bakito/adguardhome-sync", "latest"},
		{"localhost:5005/app", "localhost:5005/app", "latest"},
		{"localhost:5005/app:v1", "localhost:5005/app", "v1"},
		{"registry.example.com:5000/a/b", "registry.example.com:5000/a/b", "latest"},
		{"registry.example.com:5000/a/b:2", "registry.example.com:5000/a/b", "2"},
		{"nginx@sha256:abc", "nginx", "sha256:abc"},
		{"nginx:1@sha256:abc", "nginx", "sha256:abc"},
	} {
		var q map[string][]string
		c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
			q = r.URL.Query()
			w.Write([]byte(`{"status":"Done"}` + "\n"))
		})
		if err := c.PullImage(context.Background(), tc.ref, ""); err != nil {
			t.Fatalf("%s: %v", tc.ref, err)
		}
		if len(q["fromImage"]) != 1 || q["fromImage"][0] != tc.from || len(q["tag"]) != 1 || q["tag"][0] != tc.tag {
			t.Errorf("%s: fromImage=%v tag=%v, want %q and %q", tc.ref, q["fromImage"], q["tag"], tc.from, tc.tag)
		}
	}
}

func TestImageCallsKeepUntaggedReferences(t *testing.T) {
	var paths []string
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Write([]byte(`{"Id":"sha256:9","Config":{}}`))
	})
	if _, err := c.ImageID(context.Background(), "localhost:5005/app"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ImageConfig(context.Background(), "localhost:5005/app"); err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if p != "/images/localhost:5005/app/json" {
			t.Errorf("path %s", p)
		}
	}
}

func TestSplitRef(t *testing.T) {
	for _, tc := range []struct{ ref, repo, tag string }{
		{"nginx", "nginx", "latest"},
		{"nginx:latest", "nginx", "latest"},
		{"nginx:1.25-alpine", "nginx", "1.25-alpine"},
		{"library/nginx", "library/nginx", "latest"},
		{"ghcr.io/o/i", "ghcr.io/o/i", "latest"},
		{"ghcr.io/o/i:1", "ghcr.io/o/i", "1"},
		{"localhost:5005/app", "localhost:5005/app", "latest"},
		{"localhost:5005/app:v1", "localhost:5005/app", "v1"},
		{"registry.example.com:5000/a/b", "registry.example.com:5000/a/b", "latest"},
		{"registry.example.com:5000/a/b/c:tag", "registry.example.com:5000/a/b/c", "tag"},
		{"GHCR.IO/Org/Img", "GHCR.IO/Org/Img", "latest"},
		{"GHCR.IO:443/Org/Img:T", "GHCR.IO:443/Org/Img", "T"},
		{"[::1]:5000/x", "[::1]:5000/x", "latest"},
		{"[::1]:5000/x:v2", "[::1]:5000/x", "v2"},
		{"nginx:", "nginx", "latest"},
		{"nginx@sha256:abc", "nginx", "sha256:abc"},
		{"nginx:1.25@sha256:abc", "nginx", "sha256:abc"},
		{"localhost:5005/app@sha256:abc", "localhost:5005/app", "sha256:abc"},
		{"localhost:5005/app:v1@sha256:abc", "localhost:5005/app", "sha256:abc"},
	} {
		repo, tag := splitRef(tc.ref)
		if repo != tc.repo || tag != tc.tag {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", tc.ref, repo, tag, tc.repo, tc.tag)
		}
	}
}

func TestPullImageRegistryAuthHeader(t *testing.T) {
	for _, auth := range []string{"abc123", ""} {
		var got []string
		c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Values("X-Registry-Auth")
			w.Write([]byte(`{"status":"Done"}` + "\n"))
		})
		if err := c.PullImage(context.Background(), "ghcr.io/o/i:1", auth); err != nil {
			t.Fatal(err)
		}
		if auth == "" && len(got) != 0 {
			t.Fatalf("header must be absent for anonymous pulls, got %v", got)
		}
		if auth != "" && (len(got) != 1 || got[0] != auth) {
			t.Fatalf("got %v", got)
		}
	}
}

func TestPullErrorsNeverEchoTheAuthHeader(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"http error":   func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unauthorized", 401) },
		"stream error": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"error":"denied"}` + "\n")) },
	} {
		err := newTest(t, h).PullImage(context.Background(), "ghcr.io/o/i:1", "SECRETHEADER")
		if err == nil || strings.Contains(err.Error(), "SECRETHEADER") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestPullImageStreamError(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"Pulling"}` + "\n" + `{"error":"pull access denied"}` + "\n"))
	})
	err := c.PullImage(context.Background(), "x/y:z", "")
	if err == nil || err.Error() != "pull x/y:z: pull access denied" {
		t.Fatalf("got %v", err)
	}
}

func TestImageID(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images/localhost:5000/app:latest/json" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Write([]byte(`{"Id":"sha256:222"}`))
	})
	id, err := c.ImageID(context.Background(), "localhost:5000/app:latest")
	if err != nil || id != "sha256:222" {
		t.Fatalf("%q %v", id, err)
	}
}

func TestImageIsLocal(t *testing.T) {
	bodies := map[string]string{
		"classic-local":  `{"Id":"x","RepoDigests":[]}`,
		"classic-null":   `{"Id":"x","RepoDigests":null}`,
		"classic-pulled": `{"Id":"x","RepoDigests":["app@sha256:111"]}`,
		"cd-local":       `{"Id":"x","RepoDigests":["app@sha256:x"],"Identity":{"Build":[{"Ref":"r","CreatedAt":"t"}]}}`,
		"cd-pulled":      `{"Id":"x","RepoDigests":["app@sha256:111"],"Identity":{"Pull":[{"Repository":"docker.io/library/busybox"}]}}`,
		"cd-both":        `{"Id":"x","Identity":{"Pull":[{"Repository":"r"}],"Build":[{"Ref":"r"}]}}`,
		"cd-empty":       `{"Id":"x","RepoDigests":[],"Identity":{}}`,
		"cd-null-lists":  `{"Id":"x","Identity":{"Pull":null,"Build":null}}`,
		"cd-unknown":     `{"Id":"x","Future":1,"Identity":{"Build":[{"Ref":"r","New":{"a":1}}],"Other":true}}`,
	}
	want := map[string]bool{"classic-local": true, "classic-null": true, "classic-pulled": false,
		"cd-local": true, "cd-pulled": false, "cd-both": false, "cd-empty": false, "cd-null-lists": false, "cd-unknown": true}
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/images/"), "/json")
		body, ok := bodies[name]
		if !ok {
			http.Error(w, "no such image", http.StatusNotFound)
			return
		}
		w.Write([]byte(body))
	})
	for name, exp := range want {
		got, err := c.ImageIsLocal(context.Background(), name)
		if err != nil || got != exp {
			t.Errorf("%s: got %v, %v want %v", name, got, err, exp)
		}
	}
	var apiErr *APIError
	if _, err := c.ImageIsLocal(context.Background(), "missing"); !errors.As(err, &apiErr) || apiErr.Status != 404 {
		t.Fatalf("got %v", err)
	}
}

func TestCreateContainer(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/create" || r.URL.Query().Get("name") != "web" {
			t.Errorf("unexpected %s", r.URL)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["Image"] != "nginx:latest" {
			t.Errorf("body %v", body)
		}
		w.WriteHeader(201)
		w.Write([]byte(`{"Id":"newid"}`))
	})
	id, err := c.CreateContainer(context.Background(), "web", map[string]any{"Image": "nginx:latest"})
	if err != nil || id != "newid" {
		t.Fatalf("%q %v", id, err)
	}
}

func TestLifecycleCalls(t *testing.T) {
	var seen []string
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		w.WriteHeader(204)
	})
	ctx := context.Background()
	c.StartContainer(ctx, "id1")
	c.StopContainer(ctx, "id1")
	c.RenameContainer(ctx, "id1", "web_old")
	c.RemoveContainer(ctx, "id1", true)
	c.ConnectNetwork(ctx, "net1", "id1", map[string]any{"Aliases": []string{"a"}})
	want := []string{
		"POST /containers/id1/start",
		"POST /containers/id1/stop",
		"POST /containers/id1/rename?name=web_old",
		"DELETE /containers/id1?force=true&v=false",
		"POST /networks/net1/connect",
	}
	if len(seen) != len(want) {
		t.Fatalf("seen %v", seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("call %d: got %q want %q", i, seen[i], want[i])
		}
	}
}

func TestAPIError(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"No such container"}`, 404)
	})
	err := c.StartContainer(context.Background(), "nope")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 {
		t.Fatalf("got %v", err)
	}
}

func TestImageConfig(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/images/sha256:111/json" {
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
		w.Write([]byte(`{"Id":"sha256:111","Config":{"Env":["A=1"],"Cmd":["run"]}}`))
	})
	cfg, err := c.ImageConfig(context.Background(), "sha256:111")
	if err != nil {
		t.Fatal(err)
	}
	if env, _ := cfg["Env"].([]any); len(env) != 1 || env[0] != "A=1" || cfg["Cmd"] == nil {
		t.Fatalf("%v", cfg)
	}
}

func TestImageConfigNotFound(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such image", 404)
	})
	var ae *APIError
	if _, err := c.ImageConfig(context.Background(), "gone"); !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("%v", err)
	}
}

func TestInspectContainerState(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Id":"abc123","Name":"/web","RestartCount":2,
		"State":{"Status":"running","Running":true,"Paused":false,"Restarting":false,"OOMKilled":false,"Dead":false,
		"Pid":4242,"ExitCode":0,"Error":"","StartedAt":"2025-01-01T10:00:00.000000000Z","FinishedAt":"0001-01-01T00:00:00Z",
		"Health":{"Status":"starting","FailingStreak":1,"Log":[{"Start":"2025-01-01T10:00:01Z","End":"2025-01-01T10:00:01Z","ExitCode":1,"Output":"nope"}]}},
		"Config":{"Image":"nginx:latest"},"HostConfig":{},"Mounts":[],"NetworkSettings":{"Networks":{}}}`))
	})
	info, err := c.InspectContainer(context.Background(), "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if !info.State.Running || info.State.Restarting || info.State.Status != "running" || info.State.ExitCode != 0 || info.RestartCount != 2 {
		t.Fatalf("%+v", info)
	}
	if info.State.Health == nil || info.State.Health.Status != "starting" {
		t.Fatalf("health: %+v", info.State.Health)
	}
}

func TestInspectContainerWithoutHealthcheckHasNilHealth(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Id":"abc","State":{"Status":"exited","Running":false,"Restarting":false,"ExitCode":3},"RestartCount":0}`))
	})
	info, err := c.InspectContainer(context.Background(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Health != nil || info.State.Running || info.State.ExitCode != 3 || info.State.Status != "exited" {
		t.Fatalf("%+v", info.State)
	}
}

func TestRemoveImage(t *testing.T) {
	var seen string
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Method + " " + r.URL.RequestURI()
		w.Write([]byte(`[{"Deleted":"sha256:111"}]`))
	})
	if err := c.RemoveImage(context.Background(), "sha256:111"); err != nil {
		t.Fatal(err)
	}
	if want := "DELETE /images/sha256:111?force=false&noprune=false"; seen != want {
		t.Fatalf("got %q want %q", seen, want)
	}
}

func TestRemoveImageConflictIsAnAPIError(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"conflict: unable to delete, image is being used by running container"}`, 409)
	})
	var ae *APIError
	if err := c.RemoveImage(context.Background(), "sha256:111"); !errors.As(err, &ae) || ae.Status != 409 {
		t.Fatalf("%v", err)
	}
}

func TestImageRepoTags(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/images/sha256:111/json" {
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
		w.Write([]byte(`{"Id":"sha256:111","RepoTags":["web:latest","web:1"]}`))
	})
	tags, err := c.ImageRepoTags(context.Background(), "sha256:111")
	if err != nil || len(tags) != 2 || tags[0] != "web:latest" {
		t.Fatalf("%v %v", tags, err)
	}
	c = newTest(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"Id":"sha256:111","RepoTags":null}`)) })
	if tags, err := c.ImageRepoTags(context.Background(), "sha256:111"); err != nil || len(tags) != 0 {
		t.Fatalf("%v %v", tags, err)
	}
}

func TestHostName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/info" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Write([]byte(`{"Name":"web-01","ID":"x"}`))
	}))
	defer srv.Close()
	got, err := NewWithBase(srv.URL, srv.Client()).HostName(context.Background())
	if err != nil || got != "web-01" {
		t.Fatalf("got %q, %v", got, err)
	}
}
