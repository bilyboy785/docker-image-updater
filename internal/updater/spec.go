package updater

import (
	"reflect"
	"sort"
	"strings"

	"github.com/bilyboy785/docker-image-updater/internal/docker"
)

// networkAttach is a secondary network the new container must be connected to after creation.
type networkAttach struct {
	Network  string
	Endpoint map[string]any
}

// createSpec is the payload to create a container plus the networks to attach afterwards.
type createSpec struct {
	Body  map[string]any
	Extra []networkAttach
}

const composeImageLabel = "com.docker.compose.image"

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// buildCreateSpec turns the inspect data of a running container into the payload needed to
// recreate it. It never mutates its input.
func buildCreateSpec(info *docker.ContainerInfo, newImageID string) createSpec {
	body := cloneMap(info.Config)
	hc := cloneMap(info.HostConfig)
	preserveVolumes(info, body, hc)

	shortID := info.ID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	if h, _ := body["Hostname"].(string); h == shortID {
		delete(body, "Hostname") // the default hostname is the old container id
	}
	if labels := asMap(body["Labels"]); labels != nil {
		if _, ok := labels[composeImageLabel]; ok {
			labels = cloneMap(labels)
			labels[composeImageLabel] = newImageID // keeps `docker compose up` from recreating again
			body["Labels"] = labels
		}
	}
	body["HostConfig"] = hc

	spec := createSpec{Body: body}
	mode, _ := hc["NetworkMode"].(string)
	if strings.HasPrefix(mode, "container:") {
		// the daemon copies these from the target container; sending them back is rejected
		delete(body, "Hostname")
		delete(body, "Domainname")
		delete(body, "ExposedPorts")
	}
	networks := info.NetworkSettings.Networks
	if mode == "host" || mode == "none" || strings.HasPrefix(mode, "container:") || len(networks) == 0 {
		return spec
	}
	names := make([]string, 0, len(networks))
	for n := range networks {
		names = append(names, n)
	}
	sort.Strings(names)
	primary := mode
	if primary == "default" {
		primary = "bridge"
	}
	if _, ok := networks[primary]; !ok {
		primary = names[0]
	}
	body["NetworkingConfig"] = map[string]any{
		"EndpointsConfig": map[string]any{primary: cleanEndpoint(networks[primary], shortID)},
	}
	for _, n := range names {
		if n != primary {
			spec.Extra = append(spec.Extra, networkAttach{Network: n, Endpoint: cleanEndpoint(networks[n], shortID)})
		}
	}
	return spec
}

// cleanEndpoint keeps only the user-defined endpoint settings, not runtime state.
func cleanEndpoint(ep map[string]any, shortID string) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"Links", "IPAMConfig", "DriverOpts"} {
		if v, ok := ep[k]; ok && v != nil {
			out[k] = v
		}
	}
	if aliases, ok := ep["Aliases"].([]any); ok {
		var kept []any
		for _, a := range aliases {
			if s, _ := a.(string); s != shortID {
				kept = append(kept, a)
			}
		}
		if len(kept) > 0 {
			out["Aliases"] = kept
		}
	}
	return out
}

// preserveVolumes re-binds volumes that were not explicitly declared in HostConfig (anonymous
// volumes created by `docker run -v /path` or a Dockerfile VOLUME). Without it the new container
// would get a fresh empty volume and the data would be orphaned.
func preserveVolumes(info *docker.ContainerInfo, cfg, hc map[string]any) {
	covered := map[string]bool{}
	binds, _ := hc["Binds"].([]any)
	binds = append([]any(nil), binds...) // never write into the input's backing array
	for _, b := range binds {
		if s, ok := b.(string); ok {
			if parts := strings.Split(s, ":"); len(parts) >= 2 {
				covered[parts[1]] = true
			}
		}
	}
	if ms, ok := hc["Mounts"].([]any); ok {
		cloned := make([]any, len(ms))
		for i, m := range ms {
			cloned[i] = m
			mm := asMap(m)
			if mm == nil {
				continue
			}
			target, _ := mm["Target"].(string)
			if target == "" {
				continue
			}
			if src, _ := mm["Source"].(string); src == "" && mm["Type"] == "volume" {
				// Without a source Docker would create a fresh anonymous volume: pin the existing one.
				for _, im := range info.Mounts {
					if im.Type == "volume" && im.Name != "" && im.Destination == target {
						mm = cloneMap(mm)
						mm["Source"] = im.Name
						cloned[i] = mm
						break
					}
				}
			}
			covered[target] = true
		}
		hc["Mounts"] = cloned
	}
	vols := cloneMap(asMap(cfg["Volumes"]))
	changed := false
	for _, m := range info.Mounts {
		if m.Type != "volume" || m.Name == "" || covered[m.Destination] {
			continue
		}
		bind := m.Name + ":" + m.Destination
		if !m.RW {
			bind += ":ro"
		}
		binds = append(binds, bind)
		delete(vols, m.Destination)
		changed = true
	}
	if !changed {
		return
	}
	hc["Binds"] = binds
	if len(vols) > 0 {
		cfg["Volumes"] = vols
	} else {
		delete(cfg, "Volumes")
	}
}

// isEmptyValue reports whether v is nil or an empty string, slice or map.
func isEmptyValue(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.String, reflect.Slice, reflect.Map, reflect.Array:
		return rv.Len() == 0
	}
	return false
}

// sameValue compares two config values, treating nil and empty values as equal.
func sameValue(a, b any) bool {
	if isEmptyValue(a) && isEmptyValue(b) {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// stripImageDefaults removes from a create body everything that merely repeats the defaults of
// the old image (imgCfg, the Config of that image). `docker inspect` returns the image and
// container settings merged, so sending them back verbatim would pin the old image's defaults
// (Env, Cmd, labels...) and override those of the new image. Values the user set explicitly
// are kept. Only the top level of body is edited in place; nested maps and slices are cloned
// before being changed, and imgCfg is never modified. A nil imgCfg leaves body unchanged.
func stripImageDefaults(body, imgCfg map[string]any) {
	if imgCfg == nil {
		return
	}
	if env, ok := body["Env"].([]any); ok {
		image := map[any]bool{}
		for _, e := range asSlice(imgCfg["Env"]) {
			image[e] = true
		}
		var kept []any
		for _, e := range env {
			if !image[e] {
				kept = append(kept, e)
			}
		}
		if len(kept) > 0 {
			body["Env"] = kept
		} else {
			delete(body, "Env")
		}
	}
	if labels := asMap(body["Labels"]); labels != nil {
		image := asMap(imgCfg["Labels"])
		kept := map[string]any{}
		for k, v := range labels {
			if iv, ok := image[k]; !ok || iv != v {
				kept[k] = v
			}
		}
		if len(kept) > 0 {
			body["Labels"] = kept
		} else {
			delete(body, "Labels")
		}
	}
	for _, key := range []string{"ExposedPorts", "Volumes"} {
		m := asMap(body[key])
		if m == nil {
			continue
		}
		image := asMap(imgCfg[key])
		kept := map[string]any{}
		for k, v := range m {
			if _, ok := image[k]; !ok {
				kept[k] = v
			}
		}
		if len(kept) > 0 {
			body[key] = kept
		} else {
			delete(body, key)
		}
	}
	entrypointKept := false
	if v, ok := body["Entrypoint"]; ok {
		if sameValue(v, imgCfg["Entrypoint"]) {
			delete(body, "Entrypoint")
		} else {
			entrypointKept = true
		}
	}
	for _, key := range []string{"Cmd", "WorkingDir", "User", "StopSignal", "Healthcheck"} {
		if key == "Cmd" && entrypointKept {
			continue // a custom entrypoint changes how Cmd is interpreted: keep it as inspected
		}
		if v, ok := body[key]; ok && sameValue(v, imgCfg[key]) {
			delete(body, key)
		}
	}
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}
