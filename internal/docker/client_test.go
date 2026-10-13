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
		if r.Method != "POST" || r.URL.Path != "/images/create" || r.URL.Query().Get("fromImage") != "nginx:latest" {
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
		w.Write([]byte(`{"status":"Pulling"}` + "\n" + `{"status":"Done"}` + "\n"))
	})
	if err := c.PullImage(context.Background(), "nginx:latest", ""); err != nil {
		t.Fatal(err)
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
