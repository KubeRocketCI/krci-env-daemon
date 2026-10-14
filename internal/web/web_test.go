package web

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/KubeRocketCI/krci-env-daemon/internal/cluster"
	"github.com/KubeRocketCI/krci-env-daemon/internal/envstate"
)

type hosts map[string]string

func (h hosts) HostEnv(host string) string { return h[host] }

type fakeState struct {
	touched  []string
	asleep   []string
	statuses int
}

func (f *fakeState) Touch(ns, why string) { f.touched = append(f.touched, ns+": "+why) }

func (f *fakeState) Status(string) envstate.StatusDoc {
	f.statuses++

	return envstate.StatusDoc{Components: []envstate.Component{
		{Name: "krci-portal", Ready: true}, {Name: "el-gitlab"},
	}}
}

func (f *fakeState) WakeNow(ns string) bool {
	if ns != "team-a" {
		return false
	}

	f.touched = append(f.touched, ns+": admin")

	return true
}

func (f *fakeState) SleepNow(ns string) bool {
	if ns != "team-a" {
		return false
	}

	f.asleep = append(f.asleep, ns)

	return true
}

func (f *fakeState) Snapshot() []envstate.EnvView {
	return []envstate.EnvView{{Env: "team-a", Active: true, Ready: 1, Total: 2, Wakeups: 3, LastSeenUnix: 1700000000, ColdStartSeconds: 22.5}}
}

func do(h http.Handler, method, host, target string, headers ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.Host = host

	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	return w
}

func TestStatus(t *testing.T) {
	st := &fakeState{}
	h := Public(st, hosts{"portal.team-a.example.com": "team-a"})

	w := do(h, http.MethodGet, "portal.team-a.example.com", PathPrefix+"status")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "application/json", w.Header().Get("Content-Type"))
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	require.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
	require.Equal(t, "default-src 'none'; frame-ancestors 'none'", w.Header().Get("Content-Security-Policy"))
	require.Equal(t, []string{"team-a: waiting page"}, st.touched)

	var doc envstate.StatusDoc
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &doc))
	require.Len(t, doc.Components, 2)
	require.NotContains(t, w.Body.String(), "team-a", "the answer names no namespace")

	require.Equal(t, http.StatusNotFound, do(h, http.MethodGet, "unknown.example.com", PathPrefix+"status").Code)
	require.Len(t, st.touched, 1)
}

func TestStatusIsCachedAndRateLimitedPerEnvironment(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st := &fakeState{}
	h := public(st, hosts{"a.example.com": "team-a", "b.example.com": "team-b"}, func() time.Time { return now })

	for range statusBurst {
		require.Equal(t, http.StatusOK, do(h, http.MethodGet, "a.example.com", PathPrefix+"status").Code)
	}

	require.Equal(t, 1, st.statuses, "one lookup per environment within the cache TTL")

	w := do(h, http.MethodGet, "a.example.com", PathPrefix+"status")
	require.Equal(t, http.StatusTooManyRequests, w.Code)
	require.Equal(t, "1", w.Header().Get("Retry-After"))
	require.Equal(t, http.StatusOK, do(h, http.MethodGet, "b.example.com", PathPrefix+"status").Code)

	now = now.Add(time.Second)
	require.Equal(t, http.StatusOK, do(h, http.MethodGet, "a.example.com", PathPrefix+"status").Code)
	require.Equal(t, 3, st.statuses, "the cache expired for team-a")
}

func TestPublicRefusesEverythingElse(t *testing.T) {
	h := Public(&fakeState{}, hosts{"a.example.com": "team-a"})

	w := do(h, http.MethodPost, "a.example.com", PathPrefix+"status")
	require.Equal(t, http.StatusMethodNotAllowed, w.Code)
	require.Equal(t, "GET, HEAD", w.Header().Get("Allow"))

	for _, target := range []string{
		PathPrefix + "assets/unknown.js",
		PathPrefix + "assets/..%2fpublic.go",
		PathPrefix + "status/extra",
		"/admin/envs",
		"/metrics",
	} {
		require.Equal(t, http.StatusNotFound, do(h, http.MethodGet, "a.example.com", target).Code, target)
	}
}

func TestAssets(t *testing.T) {
	h := Public(&fakeState{}, hosts{})

	for name, typ := range assetTypes {
		w := do(h, http.MethodGet, "any.example.com", PathPrefix+"assets/"+name)
		require.Equal(t, http.StatusOK, w.Code, name)
		require.Equal(t, typ, w.Header().Get("Content-Type"), name)
		require.NotZero(t, w.Body.Len(), name)
	}

	svg := do(h, http.MethodGet, "any.example.com", PathPrefix+"assets/logo.svg")
	require.Equal(t, "default-src 'none'; style-src 'unsafe-inline'", svg.Header().Get("Content-Security-Policy"))
	require.Equal(t, http.StatusOK, do(h, http.MethodGet, "any.example.com", "/healthz").Code)
}

// TestWaitingPageContract ties the page that Envoy serves and its script to what the
// daemon serves: the path prefix, the status URL and the asset allowlist.
func TestWaitingPageContract(t *testing.T) {
	page, err := os.ReadFile("../../deploy-templates/files/waiting-page.html")
	require.NoError(t, err)

	wanted := map[string]bool{}
	for _, m := range regexp.MustCompile(`"(/\.[a-z-]+/)assets/([^"]+)"`).FindAllStringSubmatch(string(page), -1) {
		require.Equal(t, PathPrefix, m[1])

		wanted[m[2]] = true
	}

	require.Equal(t, slices.Sorted(maps.Keys(assetTypes)), slices.Sorted(maps.Keys(wanted)), "assets the page loads")

	embedded, err := fs.ReadDir(assetFS, "assets")
	require.NoError(t, err)

	var names []string
	for _, e := range embedded {
		names = append(names, e.Name())
	}

	require.Equal(t, slices.Sorted(maps.Keys(assetTypes)), names, "embedded assets")

	script, err := fs.ReadFile(assetFS, "assets/wake.js")
	require.NoError(t, err)
	require.Contains(t, string(script), `var STATUS = "`+PathPrefix+`status";`)
	require.Contains(t, string(script), fmt.Sprintf("var MAX_COMPONENTS = %d;", cluster.MaxTargets), "the page shows every workload an environment can have")
}

func TestAdmin(t *testing.T) {
	st := &fakeState{}
	h := AdminHandler(st)

	envs := do(h, http.MethodGet, "localhost", "/admin/envs")
	require.Equal(t, http.StatusOK, envs.Code)
	require.Contains(t, envs.Body.String(), `"env": "team-a"`)

	require.Equal(t, http.StatusOK, do(h, http.MethodPost, "localhost", "/admin/sleep?env=team-a").Code)
	require.Equal(t, http.StatusNotFound, do(h, http.MethodPost, "localhost", "/admin/sleep?env=unknown").Code)
	require.Equal(t, http.StatusOK, do(h, http.MethodPost, "localhost", "/admin/wake?env=team-a").Code)
	require.Equal(t, http.StatusNotFound, do(h, http.MethodPost, "localhost", "/admin/wake?env=unknown").Code)
	require.Equal(t, http.StatusMethodNotAllowed, do(h, http.MethodGet, "localhost", "/admin/wake?env=team-a").Code)
	require.Equal(t, http.StatusForbidden,
		do(h, http.MethodPost, "localhost", "/admin/sleep?env=team-a", "Origin", "https://attacker.example.com").Code,
		"a web page cannot use an open port-forward")
	require.Equal(t, http.StatusMisdirectedRequest, do(h, http.MethodGet, "attacker.example.com:8082", "/admin/envs").Code,
		"a DNS-rebinding page sends its own host")

	for _, host := range []string{"localhost:18082", "127.0.0.1:8082", "[::1]:8082", "localhost"} {
		require.Equal(t, http.StatusOK, do(h, http.MethodGet, host, "/admin/envs").Code, host)
	}

	require.Equal(t, []string{"team-a"}, st.asleep)
	require.Equal(t, []string{"team-a: admin"}, st.touched)
}

func TestMetrics(t *testing.T) {
	w := do(MetricsHandler(&fakeState{}), http.MethodGet, "localhost", "/metrics")
	require.Equal(t, http.StatusOK, w.Code)

	body := w.Body.String()
	for _, line := range []string{
		"# TYPE krci_env_active gauge",
		`krci_env_active{env="team-a"} 1`,
		`krci_env_components_ready{env="team-a"} 1`,
		`krci_env_components{env="team-a"} 2`,
		`krci_env_last_activity_timestamp_seconds{env="team-a"} 1.7e+09`,
		"# TYPE krci_env_wakeups_total counter",
		`krci_env_wakeups_total{env="team-a"} 3`,
		`krci_env_cold_start_seconds{env="team-a"} 22.5`,
	} {
		require.Contains(t, body, line+"\n")
	}
}
