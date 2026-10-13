package updater

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/bilyboy785/docker-image-updater/internal/docker"
)

func infoFrom(t *testing.T, js string) *docker.ContainerInfo {
	t.Helper()
	var info docker.ContainerInfo
	if err := json.Unmarshal([]byte(js), &info); err != nil {
		t.Fatal(err)
	}
	return &info
}

const composeInfo = `{
 "Id":"0123456789abcdef0123","Name":"/proj-web-1",
 "Config":{"Hostname":"0123456789ab","Image":"nginx:latest",
   "Labels":{"com.docker.compose.project":"proj","com.docker.compose.image":"sha256:old"}},
 "HostConfig":{"NetworkMode":"proj_default","Binds":["data:/data:rw"]},
 "Mounts":[{"Type":"volume","Name":"data","Destination":"/data","RW":true}],
 "NetworkSettings":{"Networks":{
   "proj_default":{"Aliases":["web","0123456789ab"],"IPAMConfig":null,"EndpointID":"e1","IPAddress":"172.18.0.2","MacAddress":"aa:bb"},
   "proj_back":{"Aliases":["web"],"IPAMConfig":{"IPv4Address":"10.0.0.5"},"EndpointID":"e2"}}}}`

func TestComposeContainer(t *testing.T) {
	spec := buildCreateSpec(infoFrom(t, composeInfo), "sha256:new")
	if spec.Body["Image"] != "nginx:latest" {
		t.Fatalf("image %v", spec.Body["Image"])
	}
	if _, ok := spec.Body["Hostname"]; ok {
		t.Error("default hostname (old container id) must be dropped")
	}
	labels := spec.Body["Labels"].(map[string]any)
	if labels["com.docker.compose.project"] != "proj" {
		t.Error("compose labels must be preserved")
	}
	if labels["com.docker.compose.image"] != "sha256:new" {
		t.Errorf("compose image label must point to the new image, got %v", labels["com.docker.compose.image"])
	}
	nc := spec.Body["NetworkingConfig"].(map[string]any)["EndpointsConfig"].(map[string]any)
	if len(nc) != 1 {
		t.Fatalf("only the primary network at creation: %v", nc)
	}
	ep := nc["proj_default"].(map[string]any)
	if !reflect.DeepEqual(ep["Aliases"], []any{"web"}) {
		t.Errorf("aliases %v (old short id must be removed)", ep["Aliases"])
	}
	for _, k := range []string{"EndpointID", "IPAddress", "MacAddress"} {
		if _, ok := ep[k]; ok {
			t.Errorf("%s must not be copied", k)
		}
	}
	if len(spec.Extra) != 1 || spec.Extra[0].Network != "proj_back" {
		t.Fatalf("extra %+v", spec.Extra)
	}
	if spec.Extra[0].Endpoint["IPAMConfig"] == nil {
		t.Error("static IP config must be preserved")
	}
}

func TestOriginalIsNotMutated(t *testing.T) {
	info := infoFrom(t, composeInfo)
	buildCreateSpec(info, "sha256:new")
	labels := info.Config["Labels"].(map[string]any)
	if labels["com.docker.compose.image"] != "sha256:old" {
		t.Fatal("buildCreateSpec mutated its input")
	}
}

func TestAnonymousVolumeIsPreserved(t *testing.T) {
	info := infoFrom(t, `{
	 "Id":"0123456789abcdef","Name":"/db",
	 "Config":{"Image":"postgres:16","Volumes":{"/var/lib/postgresql/data":{},"/other":{}}},
	 "HostConfig":{"NetworkMode":"bridge","Binds":["named:/other:rw"]},
	 "Mounts":[
	   {"Type":"volume","Name":"0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0","Destination":"/var/lib/postgresql/data","RW":true},
	   {"Type":"volume","Name":"named","Destination":"/other","RW":true}],
	 "NetworkSettings":{"Networks":{"bridge":{}}}}`)
	spec := buildCreateSpec(info, "sha256:new")
	binds := spec.Body["HostConfig"].(map[string]any)["Binds"].([]any)
	want := []any{"named:/other:rw", "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0:/var/lib/postgresql/data"}
	if !reflect.DeepEqual(binds, want) {
		t.Fatalf("binds %v", binds)
	}
	vols := spec.Body["Volumes"].(map[string]any)
	if _, ok := vols["/var/lib/postgresql/data"]; ok {
		t.Error("anonymous volume declaration must be removed so Docker does not create a fresh one")
	}
	if _, ok := vols["/other"]; !ok {
		t.Error("other declarations must be kept")
	}
}

func TestReadOnlyAnonymousVolume(t *testing.T) {
	info := infoFrom(t, `{"Id":"0123456789abcdef","Name":"/x","Config":{"Image":"i","Volumes":{"/d":{}}},
	 "HostConfig":{"NetworkMode":"bridge"},
	 "Mounts":[{"Type":"volume","Name":"anon","Destination":"/d","RW":false}],
	 "NetworkSettings":{"Networks":{"bridge":{}}}}`)
	binds := buildCreateSpec(info, "n").Body["HostConfig"].(map[string]any)["Binds"].([]any)
	if !reflect.DeepEqual(binds, []any{"anon:/d:ro"}) {
		t.Fatalf("%v", binds)
	}
}

func TestVolumeDeclaredViaMountsIsNotDuplicated(t *testing.T) {
	info := infoFrom(t, `{"Id":"0123456789abcdef","Name":"/x","Config":{"Image":"i"},
	 "HostConfig":{"NetworkMode":"bridge","Mounts":[{"Type":"volume","Source":"v","Target":"/d"}]},
	 "Mounts":[{"Type":"volume","Name":"v","Destination":"/d","RW":true}],
	 "NetworkSettings":{"Networks":{"bridge":{}}}}`)
	hc := buildCreateSpec(info, "n").Body["HostConfig"].(map[string]any)
	if _, ok := hc["Binds"]; ok {
		t.Fatalf("must not add a bind for a --mount volume: %v", hc["Binds"])
	}
}

func TestSpecialNetworkModesHaveNoNetworkingConfig(t *testing.T) {
	for _, mode := range []string{"host", "none", "container:abc"} {
		info := infoFrom(t, `{"Id":"0123456789abcdef","Name":"/x","Config":{"Image":"i"},
		 "HostConfig":{"NetworkMode":"`+mode+`"},"Mounts":[],
		 "NetworkSettings":{"Networks":{"`+mode+`":{"Aliases":null}}}}`)
		spec := buildCreateSpec(info, "n")
		if _, ok := spec.Body["NetworkingConfig"]; ok || len(spec.Extra) != 0 {
			t.Errorf("%s: unexpected networking config %+v", mode, spec)
		}
	}
}

func TestDefaultNetworkModeMapsToBridge(t *testing.T) {
	info := infoFrom(t, `{"Id":"0123456789abcdef","Name":"/x","Config":{"Image":"i"},
	 "HostConfig":{"NetworkMode":"default"},"Mounts":[],
	 "NetworkSettings":{"Networks":{"bridge":{}}}}`)
	spec := buildCreateSpec(info, "n")
	eps := spec.Body["NetworkingConfig"].(map[string]any)["EndpointsConfig"].(map[string]any)
	if _, ok := eps["bridge"]; !ok {
		t.Fatalf("%v", eps)
	}
}

func TestEmptySourceVolumeMountIsPinnedToExistingVolume(t *testing.T) {
	info := infoFrom(t, `{"Id":"0123456789abcdef","Name":"/x","Config":{"Image":"i"},
	 "HostConfig":{"NetworkMode":"bridge","Mounts":[{"Type":"volume","Target":"/d"},{"Type":"volume","Source":"v","Target":"/e"}]},
	 "Mounts":[{"Type":"volume","Name":"abc123","Destination":"/d","RW":true},
	   {"Type":"volume","Name":"v","Destination":"/e","RW":true}],
	 "NetworkSettings":{"Networks":{"bridge":{}}}}`)
	hc := buildCreateSpec(info, "n").Body["HostConfig"].(map[string]any)
	if _, ok := hc["Binds"]; ok {
		t.Fatalf("no bind expected: %v", hc["Binds"])
	}
	ms := hc["Mounts"].([]any)
	if ms[0].(map[string]any)["Source"] != "abc123" {
		t.Fatalf("source not pinned: %v", ms[0])
	}
	if ms[1].(map[string]any)["Source"] != "v" {
		t.Fatalf("named source changed: %v", ms[1])
	}
	orig := info.HostConfig["Mounts"].([]any)[0].(map[string]any)
	if _, ok := orig["Source"]; ok {
		t.Fatal("input HostConfig.Mounts was mutated")
	}
}

func TestContainerNetworkModeDropsHostnameAndPorts(t *testing.T) {
	info := infoFrom(t, `{"Id":"0123456789abcdef","Name":"/x",
	 "Config":{"Image":"i","Hostname":"somehost","Domainname":"d","ExposedPorts":{"80/tcp":{}}},
	 "HostConfig":{"NetworkMode":"container:abc"},"Mounts":[],
	 "NetworkSettings":{"Networks":{}}}`)
	spec := buildCreateSpec(info, "n")
	for _, k := range []string{"Hostname", "Domainname", "ExposedPorts", "NetworkingConfig"} {
		if _, ok := spec.Body[k]; ok {
			t.Errorf("%s must be absent", k)
		}
	}
	if len(spec.Extra) != 0 {
		t.Errorf("extra %+v", spec.Extra)
	}
	if _, ok := info.Config["Hostname"]; !ok {
		t.Error("input mutated")
	}
}

func TestBindsAreNotSharedAcrossCalls(t *testing.T) {
	info := infoFrom(t, `{"Id":"0123456789abcdef","Name":"/x","Config":{"Image":"i"},
	 "HostConfig":{"NetworkMode":"bridge","Binds":["a:/a:rw"]},
	 "Mounts":[{"Type":"volume","Name":"anon","Destination":"/d","RW":true}],
	 "NetworkSettings":{"Networks":{"bridge":{}}}}`)
	// force spare capacity so a naive append would write into the shared array
	b := make([]any, 1, 8)
	b[0] = "a:/a:rw"
	info.HostConfig["Binds"] = b
	first := buildCreateSpec(info, "n").Body["HostConfig"].(map[string]any)["Binds"]
	second := buildCreateSpec(info, "n").Body["HostConfig"].(map[string]any)["Binds"]
	if !reflect.DeepEqual(first, second) || len(first.([]any)) != 2 {
		t.Fatalf("%v vs %v", first, second)
	}
	if len(info.HostConfig["Binds"].([]any)) != 1 || b[:2][1] != nil {
		t.Fatal("input binds backing array was written")
	}
}

func strs(v ...string) []any {
	out := make([]any, len(v))
	for i, s := range v {
		out[i] = s
	}
	return out
}

func TestStripEnvKeepsUserVariables(t *testing.T) {
	body := map[string]any{"Env": strs("PATH=/old", "PG_MAJOR=16", "POSTGRES_PASSWORD=x", "CUSTOM=yes")}
	img := map[string]any{"Env": strs("PATH=/old", "PG_MAJOR=16", "POSTGRES_PASSWORD=")}
	stripImageDefaults(body, img)
	if got := body["Env"]; !reflect.DeepEqual(got, strs("POSTGRES_PASSWORD=x", "CUSTOM=yes")) {
		t.Fatalf("%v", got)
	}
}

func TestStripEnvRemovedWhenNothingRemains(t *testing.T) {
	body := map[string]any{"Env": strs("A=1")}
	stripImageDefaults(body, map[string]any{"Env": strs("A=1")})
	if _, ok := body["Env"]; ok {
		t.Fatalf("%v", body)
	}
}

func TestStripLabelsKeepsNonImageLabels(t *testing.T) {
	body := map[string]any{"Labels": map[string]any{
		"org.opencontainers.image.version": "1", "com.docker.compose.project": "p", "changed": "mine"}}
	img := map[string]any{"Labels": map[string]any{"org.opencontainers.image.version": "1", "changed": "image"}}
	stripImageDefaults(body, img)
	want := map[string]any{"com.docker.compose.project": "p", "changed": "mine"}
	if got := body["Labels"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestStripPortsAndVolumes(t *testing.T) {
	body := map[string]any{
		"ExposedPorts": map[string]any{"80/tcp": map[string]any{}, "9000/tcp": map[string]any{}},
		"Volumes":      map[string]any{"/data": map[string]any{}}}
	img := map[string]any{
		"ExposedPorts": map[string]any{"80/tcp": map[string]any{}},
		"Volumes":      map[string]any{"/data": map[string]any{}}}
	stripImageDefaults(body, img)
	if !reflect.DeepEqual(body["ExposedPorts"], map[string]any{"9000/tcp": map[string]any{}}) {
		t.Fatalf("%v", body["ExposedPorts"])
	}
	if _, ok := body["Volumes"]; ok {
		t.Fatalf("%v", body["Volumes"])
	}
}

func TestStripKeepsExposedPortsThatArePublished(t *testing.T) {
	body := map[string]any{
		"ExposedPorts": map[string]any{"80/tcp": map[string]any{}, "443/tcp": map[string]any{}, "9000/tcp": map[string]any{}},
		"HostConfig": map[string]any{"PortBindings": map[string]any{
			"80/tcp": []any{map[string]any{"HostPort": "8080"}}}}}
	img := map[string]any{"ExposedPorts": map[string]any{"80/tcp": map[string]any{}, "443/tcp": map[string]any{}}}
	stripImageDefaults(body, img)
	want := map[string]any{"80/tcp": map[string]any{}, "9000/tcp": map[string]any{}}
	if !reflect.DeepEqual(body["ExposedPorts"], want) {
		t.Fatalf("%v", body["ExposedPorts"])
	}
}

func TestStripEqualCmdAndEntrypointRemoved(t *testing.T) {
	body := map[string]any{"Entrypoint": strs("/entry.sh"), "Cmd": strs("run"), "WorkingDir": "/app",
		"User": "app", "StopSignal": "SIGTERM", "Healthcheck": map[string]any{"Test": strs("CMD", "true")}}
	img := map[string]any{"Entrypoint": strs("/entry.sh"), "Cmd": strs("run"), "WorkingDir": "/app",
		"User": "app", "StopSignal": "SIGTERM", "Healthcheck": map[string]any{"Test": strs("CMD", "true")}}
	stripImageDefaults(body, img)
	if len(body) != 0 {
		t.Fatalf("%v", body)
	}
}

func TestStripNilAndEmptyAreEquivalent(t *testing.T) {
	body := map[string]any{"Entrypoint": nil, "Cmd": strs("run"), "User": ""}
	stripImageDefaults(body, map[string]any{"Cmd": strs("run")})
	if len(body) != 0 {
		t.Fatalf("%v", body)
	}
}

func TestStripCustomCmdKept(t *testing.T) {
	body := map[string]any{"Cmd": strs("custom")}
	stripImageDefaults(body, map[string]any{"Cmd": strs("run")})
	if !reflect.DeepEqual(body["Cmd"], strs("custom")) {
		t.Fatalf("%v", body)
	}
}

func TestStripCustomEntrypointKeepsCmd(t *testing.T) {
	body := map[string]any{"Entrypoint": strs("/mine"), "Cmd": strs("run")}
	stripImageDefaults(body, map[string]any{"Entrypoint": strs("/entry.sh"), "Cmd": strs("run")})
	if !reflect.DeepEqual(body["Entrypoint"], strs("/mine")) || !reflect.DeepEqual(body["Cmd"], strs("run")) {
		t.Fatalf("%v", body)
	}
}

func TestStripNilImageConfigIsNoop(t *testing.T) {
	body := map[string]any{"Env": strs("A=1"), "Cmd": strs("x")}
	stripImageDefaults(body, nil)
	if !reflect.DeepEqual(body, map[string]any{"Env": strs("A=1"), "Cmd": strs("x")}) {
		t.Fatalf("%v", body)
	}
}

func TestStripNeverMutatesInputs(t *testing.T) {
	env := strs("A=1", "B=2")
	labels := map[string]any{"l": "1", "k": "2"}
	ports := map[string]any{"80/tcp": map[string]any{}}
	imgEnv := strs("A=1")
	imgLabels := map[string]any{"l": "1"}
	imgPorts := map[string]any{"80/tcp": map[string]any{}}
	img := map[string]any{"Env": imgEnv, "Labels": imgLabels, "ExposedPorts": imgPorts, "Cmd": strs("x")}
	body := map[string]any{"Env": env, "Labels": labels, "ExposedPorts": ports, "Cmd": strs("x")}
	stripImageDefaults(body, img)
	if !reflect.DeepEqual(env, strs("A=1", "B=2")) || len(labels) != 2 || len(ports) != 1 ||
		!reflect.DeepEqual(imgEnv, strs("A=1")) || len(imgLabels) != 1 || len(imgPorts) != 1 || img["Cmd"] == nil {
		t.Fatal("inputs were mutated")
	}
}
