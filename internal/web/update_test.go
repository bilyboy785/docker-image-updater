package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

const testToken = "s3cr3t-token"

// fakeUpdateAPI records what the handler asked for.
type fakeUpdateAPI struct {
	containers map[string][]string // canonical image -> names of the running containers
	lookupErr  error
	result     UpdateResult
	looked     []string
	requested  []string
}

func (f *fakeUpdateAPI) api() UpdateAPI {
	return UpdateAPI{
		Containers: func(_ context.Context, ref string) ([]string, error) {
			f.looked = append(f.looked, ref)
			return f.containers[ref], f.lookupErr
		},
		Request: func(ref string) UpdateResult {
			f.requested = append(f.requested, ref)
			return f.result
		},
	}
}

func newUpdateHandler(f *fakeUpdateAPI) http.Handler {
	return Handler(testSnap, nil, WithUpdateAPI(testToken, f.api()))
}

func postUpdate(h http.Handler, query string, headers map[string]string) *httptest.ResponseRecorder {
	return postTo(h, "/api/update"+query, headers)
}

func bearer(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

func imageQuery(ref string) string { return "?image=" + url.QueryEscape(ref) }

func TestUpdateIsDisabledWithoutToken(t *testing.T) {
	f := &fakeUpdateAPI{}
	for _, h := range []http.Handler{
		Handler(testSnap, nil),
		Handler(testSnap, nil, WithUpdateAPI("", f.api())),
	} {
		rec := postUpdate(h, imageQuery("app:1"), bearer(testToken))
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
	}
	if len(f.looked)+len(f.requested) != 0 {
		t.Fatalf("%v %v", f.looked, f.requested)
	}
}

func TestUpdateRequiresTheBearerToken(t *testing.T) {
	for name, headers := range map[string]map[string]string{
		"no header":      nil,
		"wrong token":    bearer("nope"),
		"empty token":    bearer(""),
		"prefix":         bearer(testToken[:len(testToken)-1]),
		"longer":         bearer(testToken + "x"),
		"basic scheme":   {"Authorization": "Basic " + testToken},
		"bare token":     {"Authorization": testToken},
		"other header":   {"X-Api-Key": testToken},
		"csrf header":    csrfHeader,
		"token in query": nil,
	} {
		f := &fakeUpdateAPI{containers: map[string][]string{"docker.io/library/app:1": {"app"}}}
		query := imageQuery("app:1")
		if name == "token in query" {
			query += "&token=" + testToken
		}
		rec := postUpdate(newUpdateHandler(f), query, headers)
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s: got %d %v", name, rec.Code, rec.Header())
		}
		if len(f.looked)+len(f.requested) != 0 {
			t.Errorf("%s: an unauthenticated call must not reach Docker or the queue: %v %v", name, f.looked, f.requested)
		}
		if strings.Contains(rec.Body.String(), testToken) {
			t.Errorf("%s: the token must never be echoed", name)
		}
	}
}

func TestUpdateAcceptsALowercaseBearerScheme(t *testing.T) {
	f := &fakeUpdateAPI{containers: map[string][]string{"docker.io/library/app:1": {"app"}}}
	rec := postUpdate(newUpdateHandler(f), imageQuery("app:1"), map[string]string{"Authorization": "bearer " + testToken})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestUpdateQueuesTheCanonicalImage(t *testing.T) {
	f := &fakeUpdateAPI{containers: map[string][]string{"registry.bldwebagency.fr/app:latest": {"app", "app-worker"}}, result: UpdateResult{Status: UpdateQueued}}
	rec := postUpdate(newUpdateHandler(f), imageQuery("Registry.BLDWebAgency.fr/app"), bearer(testToken))
	if rec.Code != http.StatusAccepted || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Status     string   `json:"status"`
		Image      string   `json:"image"`
		Containers []string `json:"containers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "queued" || body.Image != "registry.bldwebagency.fr/app:latest" || !reflect.DeepEqual(body.Containers, []string{"app", "app-worker"}) {
		t.Fatalf("%+v", body)
	}
	if !reflect.DeepEqual(f.requested, []string{"registry.bldwebagency.fr/app:latest"}) {
		t.Fatalf("the queue must receive the canonical reference: %v", f.requested)
	}
}

func TestUpdateWithoutRunningContainerIs404AndQueuesNothing(t *testing.T) {
	f := &fakeUpdateAPI{}
	rec := postUpdate(newUpdateHandler(f), imageQuery("app:1"), bearer(testToken))
	if rec.Code != http.StatusNotFound || len(f.requested) != 0 {
		t.Fatalf("got %d %s %v", rec.Code, rec.Body, f.requested)
	}
}

func TestUpdateRejectsMalformedImages(t *testing.T) {
	for name, query := range map[string]string{
		"missing":        "",
		"empty":          "?image=",
		"blank":          "?image=%20%20",
		"digest":         imageQuery("app@sha256:" + strings.Repeat("a", 64)),
		"space":          imageQuery("my app:1"),
		"newline":        "?image=app%0A:1",
		"shell":          imageQuery("app:1;rm -rf /"),
		"leading slash":  imageQuery("/app:1"),
		"trailing slash": imageQuery("registry.example.com/app/"),
		"trailing colon": imageQuery("app:"),
		"leading dash":   imageQuery("-app:1"),
		"too long":       imageQuery(strings.Repeat("a", 256)),
		"twice":          "?image=a:1&image=b:1",
		"undecodable":    "?image=%zz",
		"non ascii":      imageQuery("applé:1"),
	} {
		f := &fakeUpdateAPI{containers: map[string][]string{}}
		rec := postUpdate(newUpdateHandler(f), query, bearer(testToken))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d %s", name, rec.Code, rec.Body)
		}
		if len(f.looked)+len(f.requested) != 0 {
			t.Errorf("%s: %v %v", name, f.looked, f.requested)
		}
	}
}

func TestUpdateChecksTheTokenBeforeTheImage(t *testing.T) {
	rec := postUpdate(newUpdateHandler(&fakeUpdateAPI{}), "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestUpdateQueueFullIs503WithRetryAfter(t *testing.T) {
	f := &fakeUpdateAPI{containers: map[string][]string{"docker.io/library/app:1": {"app"}}, result: UpdateResult{Status: UpdateQueueFull}}
	rec := postUpdate(newUpdateHandler(f), imageQuery("app:1"), bearer(testToken))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("got %d %v", rec.Code, rec.Header())
	}
}

func TestUpdateDockerFailureIs502WithoutLeakingTheError(t *testing.T) {
	f := &fakeUpdateAPI{lookupErr: errors.New("dial unix /var/run/docker.sock: connect: permission denied")}
	rec := postUpdate(newUpdateHandler(f), imageQuery("app:1"), bearer(testToken))
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "docker.sock") || len(f.requested) != 0 {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestUpdateOnlyAcceptsPOST(t *testing.T) {
	f := &fakeUpdateAPI{containers: map[string][]string{"docker.io/library/app:1": {"app"}}}
	h := newUpdateHandler(f)
	for _, m := range []string{"GET", "PUT", "DELETE"} {
		req := httptest.NewRequest(m, "/api/update"+imageQuery("app:1"), nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: got %d", m, rec.Code)
		}
	}
	if len(f.requested) != 0 {
		t.Fatal("a non POST call must not queue anything")
	}
}

func TestUpdateDoesNotNeedTheScanCSRFHeader(t *testing.T) {
	f := &fakeUpdateAPI{containers: map[string][]string{"docker.io/library/app:1": {"app"}}}
	rec := postUpdate(newUpdateHandler(f), imageQuery("app:1"), bearer(testToken))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d", rec.Code)
	}
}
