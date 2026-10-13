// Package docker is a minimal client for the Docker Engine API over a Unix socket.
package docker

import (
	"fmt"
	"strings"
)

type Container struct {
	ID      string `json:"Id"`
	Names   []string
	Image   string
	ImageID string
	Labels  map[string]string
}

// Name returns the container name without the leading slash.
func (c Container) Name() string {
	if len(c.Names) == 0 {
		return c.ID
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

type Mount struct {
	Type        string
	Name        string
	Destination string
	RW          bool
}

// ContainerInfo keeps Config and HostConfig as generic maps so that every field the
// daemon returns is preserved when the container is recreated.
type ContainerInfo struct {
	ID              string `json:"Id"`
	Name            string
	Config          map[string]any
	HostConfig      map[string]any
	Mounts          []Mount
	NetworkSettings struct {
		Networks map[string]map[string]any
	}
}

type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("docker API error %d: %s", e.Status, e.Message)
}
