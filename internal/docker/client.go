package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Client talks to the Docker Engine API.
type Client struct {
	hc   *http.Client
	base string
}

// New returns a client talking to the Docker daemon on the given Unix socket.
func New(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{hc: &http.Client{Transport: tr}, base: "http://docker"}
}

// NewWithBase returns a client for an arbitrary HTTP base URL; tests use it to target a fake server.
func NewWithBase(base string, hc *http.Client) *Client {
	return &Client{hc: hc, base: strings.TrimRight(base, "/")}
}

func (c *Client) send(ctx context.Context, method, path string, q url.Values, body any, extra ...http.Header) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, h := range extra {
		for k, vs := range h {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(msg))}
	}
	return resp, nil
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	resp, err := c.send(ctx, method, path, q, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// ListContainers returns the running containers.
func (c *Client) ListContainers(ctx context.Context) ([]Container, error) {
	var out []Container
	err := c.do(ctx, "GET", "/containers/json", nil, nil, &out)
	return out, err
}

// InspectContainer returns the full description of a container.
func (c *Client) InspectContainer(ctx context.Context, id string) (*ContainerInfo, error) {
	var out ContainerInfo
	if err := c.do(ctx, "GET", "/containers/"+id+"/json", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PullImage pulls ref and drains the progress stream, returning any error reported in it.
// A non-empty registryAuth is sent as the X-Registry-Auth header (base64url-encoded credentials);
// it is never included in returned errors.
func (c *Client) PullImage(ctx context.Context, ref, registryAuth string) error {
	var hdr http.Header
	if registryAuth != "" {
		hdr = http.Header{"X-Registry-Auth": {registryAuth}}
	}
	resp, err := c.send(ctx, "POST", "/images/create", url.Values{"fromImage": {ref}}, nil, hdr)
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&msg); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("pull %s: %w", ref, err)
		}
		if msg.Error != "" {
			return fmt.Errorf("pull %s: %s", ref, msg.Error)
		}
	}
}

// ImageConfig returns the Config object of an image (its default Env, Cmd, Labels and so on).
func (c *Client) ImageConfig(ctx context.Context, ref string) (map[string]any, error) {
	var out struct {
		Config map[string]any
	}
	if err := c.do(ctx, "GET", "/images/"+ref+"/json", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Config, nil
}

// ImageID returns the ID of a local image.
func (c *Client) ImageID(ctx context.Context, ref string) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	if err := c.do(ctx, "GET", "/images/"+ref+"/json", nil, nil, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// ImageIsLocal reports whether an image never came from a registry: it was built locally or
// loaded from an archive, so it cannot be pulled.
//
// With the containerd image store, the image inspection has an Identity object: an image is
// local when it has build records and no pull record. Anything else, including an empty
// Identity, is not local. With the classic store there is no Identity: an image is local when it
// has no repo digest.
func (c *Client) ImageIsLocal(ctx context.Context, id string) (bool, error) {
	var out struct {
		RepoDigests []string
		Identity    *struct {
			Pull  []json.RawMessage
			Build []json.RawMessage
		}
	}
	if err := c.do(ctx, "GET", "/images/"+id+"/json", nil, nil, &out); err != nil {
		return false, err
	}
	if out.Identity != nil {
		return len(out.Identity.Pull) == 0 && len(out.Identity.Build) > 0, nil
	}
	return len(out.RepoDigests) == 0, nil
}

// ImageRepoTags returns the repository tags an image still carries (empty for a dangling image).
func (c *Client) ImageRepoTags(ctx context.Context, id string) ([]string, error) {
	var out struct {
		RepoTags []string
	}
	if err := c.do(ctx, "GET", "/images/"+id+"/json", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.RepoTags, nil
}

// CreateContainer creates a container from a full create payload and returns its ID.
func (c *Client) CreateContainer(ctx context.Context, name string, body map[string]any) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	if err := c.do(ctx, "POST", "/containers/create", url.Values{"name": {name}}, body, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// StartContainer starts a container.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	return c.do(ctx, "POST", "/containers/"+id+"/start", nil, nil, nil)
}

// StopContainer stops a container.
func (c *Client) StopContainer(ctx context.Context, id string) error {
	return c.do(ctx, "POST", "/containers/"+id+"/stop", nil, nil, nil)
}

// RenameContainer renames a container.
func (c *Client) RenameContainer(ctx context.Context, id, name string) error {
	return c.do(ctx, "POST", "/containers/"+id+"/rename", url.Values{"name": {name}}, nil, nil)
}

// RemoveContainer deletes a container but never its volumes (v=false), so data survives.
func (c *Client) RemoveContainer(ctx context.Context, id string, force bool) error {
	q := url.Values{"force": {fmt.Sprint(force)}, "v": {"false"}}
	return c.do(ctx, "DELETE", "/containers/"+id, q, nil, nil)
}

// RemoveImage deletes an image by id without force. The daemon refuses with 409 when a container
// still uses it and answers 404 when it is already gone; both come back as an *APIError.
func (c *Client) RemoveImage(ctx context.Context, id string) error {
	q := url.Values{"force": {"false"}, "noprune": {"false"}}
	return c.do(ctx, "DELETE", "/images/"+id, q, nil, nil)
}

// ConnectNetwork attaches a container to a network with the given endpoint settings.
func (c *Client) ConnectNetwork(ctx context.Context, network, containerID string, endpoint map[string]any) error {
	body := map[string]any{"Container": containerID, "EndpointConfig": endpoint}
	return c.do(ctx, "POST", "/networks/"+network+"/connect", nil, body, nil)
}
