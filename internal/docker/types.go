// Package docker is a minimal client for the Docker Engine API over a Unix socket.
package docker

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Container is one entry of the container list.
type Container struct {
	ID      string `json:"Id"`
	Names   []string
	Image   string
	ImageID string
	Labels  map[string]string
	// NetworkMode is the container's HostConfig.NetworkMode (for example "bridge" or "container:<id>").
	NetworkMode string `json:"-"`
}

// UnmarshalJSON decodes a container list entry, lifting HostConfig.NetworkMode into NetworkMode.
func (c *Container) UnmarshalJSON(b []byte) error {
	type plain Container
	aux := struct {
		*plain
		HostConfig struct {
			NetworkMode string
		}
	}{plain: (*plain)(c)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	c.NetworkMode = aux.HostConfig.NetworkMode
	return nil
}

// Name returns the container name without the leading slash.
func (c Container) Name() string {
	if len(c.Names) == 0 {
		return c.ID
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// Mount describes one mount of an inspected container.
type Mount struct {
	Type        string
	Name        string
	Destination string
	RW          bool
}

// ContainerState is the runtime state of an inspected container.
type ContainerState struct {
	Status     string // created, running, restarting, exited, ...
	Running    bool
	Restarting bool
	ExitCode   int
	// Health is nil when the container has no healthcheck.
	Health *struct {
		Status string // starting, healthy or unhealthy
	}
}

// ContainerInfo keeps Config and HostConfig as generic maps so that every field the
// daemon returns is preserved when the container is recreated.
type ContainerInfo struct {
	ID              string `json:"Id"`
	Name            string
	Config          map[string]any
	HostConfig      map[string]any
	Mounts          []Mount
	State           ContainerState
	RestartCount    int
	NetworkSettings struct {
		Networks map[string]map[string]any
	}
}

// APIError is an error response returned by the Docker daemon.
type APIError struct {
	Status  int
	Message string
}

// Error implements the error interface.
func (e *APIError) Error() string {
	return fmt.Sprintf("docker API error %d: %s", e.Status, e.Message)
}
