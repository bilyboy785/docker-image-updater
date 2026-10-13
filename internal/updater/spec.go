package updater

import (
	"sort"
	"strings"

	"github.com/bilyboy785/docker-image-updater/internal/docker"
)

type networkAttach struct {
	Network  string
	Endpoint map[string]any
}

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
