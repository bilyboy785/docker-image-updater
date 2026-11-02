package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"

	"github.com/bilyboy785/docker-image-updater/internal/registry"
)

// UpdateStatus is the outcome of a request to update the containers of an image.
type UpdateStatus int

const (
	// UpdateQueued means the request was accepted: it runs as soon as the scan in progress, if any, ends.
	UpdateQueued UpdateStatus = iota
	// UpdateQueueFull means too many images are already waiting.
	UpdateQueueFull
)

// UpdateResult is returned by UpdateAPI.Request.
type UpdateResult struct {
	Status UpdateStatus
}

// UpdateAPI is what POST /api/update needs from the updater. Both functions receive the image as
// returned by registry.CanonicalRef.
type UpdateAPI struct {
	// Containers returns the names of the running containers that use the image, asking Docker.
	Containers func(ctx context.Context, ref string) ([]string, error)
	// Request queues the update of the containers of the image.
	Request func(ref string) UpdateResult
}

// WithUpdateAPI enables POST /api/update, protected by the bearer token. An empty token keeps the
// endpoint disabled (it then answers 501).
func WithUpdateAPI(token string, api UpdateAPI) Option {
	return func(o *options) { o.updateToken, o.update = token, api }
}

// maxImageRef is the longest image reference accepted by POST /api/update.
const maxImageRef = 255

// validImageRef reports whether v looks like an image reference without digest: ASCII letters, digits and
// . _ - / : only, starting with a letter or a digit and not ending with / or :. It is a sanity check: the
// value is only compared with the images of the running containers, never used otherwise.
func validImageRef(v string) bool {
	if v == "" || len(v) > maxImageRef {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case i > 0 && strings.IndexByte("._-/:", c) >= 0:
		default:
			return false
		}
	}
	return v[len(v)-1] != '/' && v[len(v)-1] != ':'
}

// authorized reports whether the request carries the expected bearer token. The tokens are hashed first,
// so that the comparison takes the same time whatever their length.
func authorized(r *http.Request, token string) bool {
	scheme, got, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// handleUpdate serves POST /api/update?image=<reference>.
func (o *options) handleUpdate(w http.ResponseWriter, r *http.Request) {
	// The request body is never read.
	if o.updateToken == "" || o.update.Containers == nil || o.update.Request == nil {
		writeError(w, http.StatusNotImplemented, "image updates are not enabled")
		return
	}
	if !authorized(r, o.updateToken) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	values := query["image"]
	if err != nil || len(values) != 1 || !validImageRef(values[0]) {
		writeError(w, http.StatusBadRequest, "invalid image reference")
		return
	}
	ref := registry.CanonicalRef(values[0])
	names, err := o.update.Containers(r.Context(), ref)
	if err != nil {
		writeError(w, http.StatusBadGateway, "cannot list the running containers")
		return
	}
	if len(names) == 0 {
		writeError(w, http.StatusNotFound, "no running container uses this image")
		return
	}
	if o.update.Request(ref).Status == UpdateQueueFull {
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusServiceUnavailable, "too many updates are waiting, retry later")
		return
	}
	writeJSON(w, http.StatusAccepted, struct {
		Status     string   `json:"status"`
		Image      string   `json:"image"`
		Containers []string `json:"containers"`
	}{"queued", ref, names})
}
