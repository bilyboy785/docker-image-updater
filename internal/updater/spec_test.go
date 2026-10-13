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
