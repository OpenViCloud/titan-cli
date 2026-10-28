package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/OpenViCloud/titan-cli/command"
	"github.com/OpenViCloud/titan-cli/command/vm"
	"github.com/OpenViCloud/titan-cli/output"
	"github.com/OpenViCloud/titan-cli/session"
	"github.com/OpenViCloud/titan-cli/wait"
)

// The acceptance suite of the CLI: each test runs the command tree main.go builds against a fake Keycloak
// realm and a fake /v1 API.

type fakeKeycloak struct {
	server         *httptest.Server
	denyLogin      bool
	refreshRefused bool
	revokeFails    bool
	revoked        int
}

func newFakeKeycloak(t *testing.T) *fakeKeycloak {
	fake := &fakeKeycloak{}
	mux := http.NewServeMux()
	mux.HandleFunc("/realms/titan/protocol/openid-connect/auth/device", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"device_code":               "device-1",
			"user_code":                 "WXYZ-1234",
			"verification_uri":          "https://auth.example/device",
			"verification_uri_complete": "https://auth.example/device?user_code=WXYZ-1234",
			"expires_in":                600,
			"interval":                  5,
		})
	})
	mux.HandleFunc("/realms/titan/protocol/openid-connect/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("grant_type") == "refresh_token" && fake.refreshRefused {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		if fake.denyLogin {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "access_denied"})
			return
		}
		claims, _ := json.Marshal(map[string]string{"preferred_username": "alice"})
		accessToken := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
		writeJSON(w, http.StatusOK, map[string]any{"access_token": accessToken, "refresh_token": "refresh-1", "expires_in": 300})
	})
	mux.HandleFunc("/realms/titan/protocol/openid-connect/revoke", func(w http.ResponseWriter, r *http.Request) {
		if fake.revokeFails {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fake.revoked++
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

type fakeApi struct {
	server             *httptest.Server
	mustAcceptTerms    bool
	consentStatusCode  int
	tenantStatusCode   int
	tenantBody         string
	tenantCalls        int
	authorizationsSeen []string
	sshRefusal         string // a problem body the ssh route answers instead of upgrading; "" upgrades
	sshHold            bool   // keep the session open until the CLI closes it, and record how it closed
	sshCloseStatus     chan websocket.StatusCode
	sshAuthorization   string
	sshVmId            string

	// Resources of the tenant, as GET answers them.
	clusters  []map[string]any
	templates []map[string]any
	sizes     []map[string]any
	vms       []map[string]any
	networks  []map[string]any
	sshKeys   []map[string]any
	// GET /v1/tenant/vms/{id}: the answers in order, the last one repeated. Unset: the matching row of vms.
	vmReads map[string][]string
	// Status codes of successive POST /v1/tenant/vms; when used up, 202 {vmId}.
	createAnswers []int
	// A problem answered by every write instead of its success: status and body.
	writeRefusal     int
	writeRefusalBody string
	writes           []recordedWrite
	templateClusters []string
}

// recordedWrite is one request that changes something, as the fake API received it.
type recordedWrite struct {
	Method         string
	Path           string
	IdempotencyKey string
	Body           map[string]any
}

const createdVmId = "5b0e7f4c-0000-4000-8000-0000000000aa"

const sshBanner = "SSH-2.0-fake\r\n"

const acmeTenant = `{"id":"7d0c2f9e-4b1a-4c55-9a1e-3f2b8c6d1e01","name":"acme","status":"Active","maxVms":5,"maxNetworks":3,"createdAt":"2026-09-01T08:00:00Z","debtCeiling":500000}`

func newFakeApi(t *testing.T) *fakeApi {
	fake := &fakeApi{consentStatusCode: http.StatusOK, tenantStatusCode: http.StatusOK, tenantBody: acmeTenant}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/legal/consent-status", func(w http.ResponseWriter, r *http.Request) {
		fake.authorizationsSeen = append(fake.authorizationsSeen, r.Header.Get("Authorization"))
		if fake.consentStatusCode != http.StatusOK {
			w.WriteHeader(fake.consentStatusCode)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"mustAcceptTerms": fake.mustAcceptTerms, "documentKey": "terms", "documentVersion": "2026-09-01"})
	})
	mux.HandleFunc("/v1/tenant", func(w http.ResponseWriter, r *http.Request) {
		fake.tenantCalls++
		fake.authorizationsSeen = append(fake.authorizationsSeen, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fake.tenantStatusCode)
		w.Write([]byte(fake.tenantBody))
	})
	fake.vms = []map[string]any{
		{"id": "5b0e7f4c-0000-4000-8000-000000000001", "name": "web-1", "clusterId": "local-mac", "currentState": "Running",
			"desiredState": "Running", "currentExistence": "Exists", "desiredExistence": "Exists", "transition": nil, "ipAddress": "10.0.0.2"},
		{"id": "5b0e7f4c-0000-4000-8000-000000000002", "name": "db-1", "clusterId": "local-mac", "currentState": "Stopped",
			"desiredState": "Stopped", "currentExistence": "Exists", "desiredExistence": "Exists", "transition": nil, "ipAddress": nil},
	}
	fake.clusters = []map[string]any{{"id": "local-mac", "name": "Local Mac"}}
	fake.templates = []map[string]any{{"id": "tpl-ubuntu", "name": "Ubuntu-22.04-Standard", "os": "Ubuntu 22.04", "diskGb": 10, "description": nil}}
	fake.sizes = []map[string]any{{"id": "m", "name": "Medium", "cpu": json.Number("1.5"), "vcpu": 2, "memoryMb": 4096, "pricePerHour": json.Number("0.0123456789")}}
	fake.networks = []map[string]any{{"id": "9a1d0000-0000-4000-8000-000000000001", "name": "app-net", "clusterId": "local-mac"}}
	fake.sshKeys = []map[string]any{{"id": "c3c30000-0000-4000-8000-000000000001", "name": "laptop", "type": "ssh-ed25519", "fingerprint": "SHA256:abc"}}
	mux.HandleFunc("GET /v1/clusters", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, fake.clusters) })
	mux.HandleFunc("GET /v1/templates", func(w http.ResponseWriter, r *http.Request) {
		fake.templateClusters = append(fake.templateClusters, r.URL.Query().Get("clusterId"))
		writeJSON(w, http.StatusOK, fake.templates)
	})
	mux.HandleFunc("GET /v1/vm-offerings", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, fake.sizes) })
	mux.HandleFunc("GET /v1/tenant/networks", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, fake.networks) })
	mux.HandleFunc("GET /v1/tenant/ssh-keys", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, fake.sshKeys) })
	mux.HandleFunc("GET /v1/tenant/vms", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, fake.vms) })
	mux.HandleFunc("GET /v1/tenant/vms/{vmId}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("vmId")
		answers := fake.vmReads[id]
		if len(answers) > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(answers[0]))
			if len(answers) > 1 {
				fake.vmReads[id] = answers[1:]
			}
			return
		}
		for _, vm := range fake.vms {
			if vm["id"] == id {
				writeJSON(w, http.StatusOK, vm)
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"status": 404, "title": "VM not found.", "code": "VM_NOT_FOUND"})
	})
	write := func(success int, body any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			recorded := recordedWrite{Method: r.Method, Path: r.URL.Path, IdempotencyKey: r.Header.Get("Idempotency-Key")}
			json.NewDecoder(r.Body).Decode(&recorded.Body)
			fake.writes = append(fake.writes, recorded)
			if fake.writeRefusal != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(fake.writeRefusal)
				w.Write([]byte(fake.writeRefusalBody))
				return
			}
			if body == nil {
				w.WriteHeader(success)
				return
			}
			writeJSON(w, success, body)
		}
	}
	createVm := write(http.StatusAccepted, map[string]any{"vmId": createdVmId})
	mux.HandleFunc("POST /v1/tenant/vms", func(w http.ResponseWriter, r *http.Request) {
		if len(fake.createAnswers) > 0 {
			status := fake.createAnswers[0]
			fake.createAnswers = fake.createAnswers[1:]
			fake.writes = append(fake.writes, recordedWrite{Method: r.Method, Path: r.URL.Path, IdempotencyKey: r.Header.Get("Idempotency-Key")})
			w.WriteHeader(status)
			return
		}
		createVm(w, r)
	})
	mux.HandleFunc("PATCH /v1/tenant/vms/{vmId}", write(http.StatusNoContent, nil))
	mux.HandleFunc("DELETE /v1/tenant/vms/{vmId}", write(http.StatusAccepted, nil))
	mux.HandleFunc("POST /v1/tenant/vms/{vmId}/start", write(http.StatusAccepted, nil))
	mux.HandleFunc("POST /v1/tenant/vms/{vmId}/stop", write(http.StatusAccepted, nil))
	mux.HandleFunc("POST /v1/tenant/ssh-keys", write(http.StatusCreated, map[string]any{"id": "c3c30000-0000-4000-8000-0000000000bb"}))
	mux.HandleFunc("DELETE /v1/tenant/ssh-keys/{sshKeyId}", write(http.StatusNoContent, nil))
	// Like the control plane: a refusal is a JSON problem before the upgrade; a session ends with a normal close.
	mux.HandleFunc("GET /v1/tenant/vms/{vmId}/ssh", func(w http.ResponseWriter, r *http.Request) {
		fake.sshAuthorization = r.Header.Get("Authorization")
		fake.sshVmId = r.PathValue("vmId")
		if fake.sshRefusal != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write([]byte(fake.sshRefusal))
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		conn.Write(ctx, websocket.MessageBinary, []byte(sshBanner))
		if fake.sshHold {
			for {
				_, _, err := conn.Read(ctx)
				if err != nil {
					fake.sshCloseStatus <- websocket.CloseStatus(err)
					return
				}
			}
		}
		_, message, err := conn.Read(ctx)
		if err == nil {
			conn.Write(ctx, websocket.MessageBinary, message)
		}
		conn.Close(websocket.StatusNormalClosure, "the session ended")
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

type harness struct {
	t        *testing.T
	keycloak *fakeKeycloak
	api      *fakeApi
	deps     *command.Deps
	now      time.Time
	opened   []string
	stdout   *bytes.Buffer
	stderr   *bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, keycloak: newFakeKeycloak(t), api: newFakeApi(t), now: time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)}
	endpoints := session.Endpoints{
		Api:    h.api.server.URL,
		Auth:   h.keycloak.server.URL + "/realms/titan",
		Portal: "https://portal.example",
	}
	h.stdout = &bytes.Buffer{}
	h.stderr = &bytes.Buffer{}
	h.deps = &command.Deps{
		Endpoints: endpoints,
		Sessions: &session.Manager{
			Endpoints: endpoints,
			Store:     session.Store{Dir: filepath.Join(t.TempDir(), "openvicloud")},
			Keycloak: session.Keycloak{
				Issuer: endpoints.Auth,
				HTTP:   http.DefaultClient,
				Sleep:  func(d time.Duration) { h.now = h.now.Add(d) },
				Now:    func() time.Time { return h.now },
			},
		},
		Printer: &output.Printer{Out: h.stdout, Err: h.stderr},
		HTTP:    http.DefaultClient,
		Version: "0.1.0-test",
		OpenBrowser: func(url string) error {
			h.opened = append(h.opened, url)
			return nil
		},
		Stdin: strings.NewReader(""),
		Clock: wait.Clock{
			Now:   func() time.Time { return h.now },
			Sleep: func(d time.Duration) { h.now = h.now.Add(d) },
		},
	}
	return h
}

func (h *harness) run(args ...string) error {
	h.stdout.Reset()
	h.stderr.Reset()
	root := newRootCommand(h.deps)
	root.SetArgs(args)
	root.SetOut(h.stdout)
	root.SetErr(h.stderr)
	return root.ExecuteContext(context.Background())
}

func (h *harness) login() {
	err := h.run("auth", "login")
	if err != nil {
		h.t.Fatalf("login failed: %v", err)
	}
}

func mustContain(t *testing.T, label string, text string, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("%s does not contain %q:\n%s", label, want, text)
	}
}

// US-CLI-01

func TestAuthLogin_ShowsThePageAndTheCode_ThenWhoIsLoggedIn(t *testing.T) {
	h := newHarness(t)

	err := h.run("auth", "login")

	if err != nil {
		t.Fatalf("login failed: %v", err)
	}
	mustContain(t, "stderr", h.stderr.String(), "https://auth.example/device?user_code=WXYZ-1234")
	mustContain(t, "stderr", h.stderr.String(), "WXYZ-1234")
	if len(h.opened) != 1 || h.opened[0] != "https://auth.example/device?user_code=WXYZ-1234" {
		t.Fatalf("browser opened %v", h.opened)
	}
	mustContain(t, "stdout", h.stdout.String(), "Logged in to "+h.api.server.URL+" as alice")
}

func TestAuthLogin_BrowserCannotOpen_TheLinkIsStillShown(t *testing.T) {
	h := newHarness(t)
	h.deps.OpenBrowser = func(string) error { return errors.New("no display") }

	err := h.run("auth", "login")

	if err != nil {
		t.Fatalf("login failed: %v", err)
	}
	mustContain(t, "stderr", h.stderr.String(), "https://auth.example/device?user_code=WXYZ-1234")
}

func TestAuthLogin_RefusedOnTheWeb_FailsAndLeavesNoSession(t *testing.T) {
	h := newHarness(t)
	h.keycloak.denyLogin = true

	err := h.run("auth", "login")

	if err == nil {
		t.Fatal("a refused login succeeded")
	}
	mustContain(t, "error", err.Error(), "refused")
	statusErr := h.run("auth", "status")
	if statusErr == nil {
		t.Fatal("auth status says logged in after a refused login")
	}
}

func TestAuthStatus_LoggedIn_ShowsTheUserAndTheEndpoints(t *testing.T) {
	h := newHarness(t)
	h.login()

	err := h.run("auth", "status")

	if err != nil {
		t.Fatalf("status failed: %v", err)
	}
	mustContain(t, "stdout", h.stdout.String(), "alice")
	mustContain(t, "stdout", h.stdout.String(), h.api.server.URL)
	mustContain(t, "stdout", h.stdout.String(), h.keycloak.server.URL+"/realms/titan")
}

func TestAuthStatus_NotLoggedIn_IsAnError(t *testing.T) {
	h := newHarness(t)

	err := h.run("auth", "status")

	if err == nil {
		t.Fatal("auth status succeeded with no session")
	}
	mustContain(t, "error", err.Error(), "openvicloud auth login")
}

func TestAuthLogout_RevokesAndForgetsTheSession(t *testing.T) {
	h := newHarness(t)
	h.login()

	err := h.run("auth", "logout")

	if err != nil {
		t.Fatalf("logout failed: %v", err)
	}
	if h.keycloak.revoked != 1 {
		t.Fatalf("revoked %d times, want 1", h.keycloak.revoked)
	}
	tenantErr := h.run("tenant", "get")
	if tenantErr == nil {
		t.Fatal("tenant get worked after logout")
	}
}

func TestAuthLogout_RevokeFails_ForgetsLocallyAndWarns(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.keycloak.revokeFails = true

	err := h.run("auth", "logout")

	if err == nil {
		t.Fatal("a logout the server did not confirm reported success")
	}
	mustContain(t, "error", err.Error(), "may still be valid")
	statusErr := h.run("auth", "status")
	if statusErr == nil {
		t.Fatal("the local session survived the logout")
	}
}

func TestCommand_SessionExpired_SaysToLogInAgain(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.keycloak.refreshRefused = true
	h.now = h.now.Add(time.Hour)

	err := h.run("tenant", "get")

	if err == nil {
		t.Fatal("a command ran on an expired session")
	}
	mustContain(t, "error", err.Error(), "openvicloud auth login")
	if h.api.tenantCalls != 0 {
		t.Fatal("called the API with an expired session")
	}
}

func TestCommand_SessionOfAnotherEndpoint_IsNotSentThere(t *testing.T) {
	h := newHarness(t)
	h.login()
	other := newFakeApi(t)
	h.deps.Endpoints.Api = other.server.URL
	h.deps.Sessions.Endpoints.Api = other.server.URL

	err := h.run("tenant", "get")

	if err == nil {
		t.Fatal("used the session of another endpoint")
	}
	mustContain(t, "error", err.Error(), "openvicloud auth login")
	if len(other.authorizationsSeen) != 0 {
		t.Fatalf("sent %d requests with a token to the other endpoint", len(other.authorizationsSeen))
	}
}

// US-CLI-02

func TestCommand_TermsNotAccepted_StopsWithThePortalLink(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.api.mustAcceptTerms = true

	err := h.run("tenant", "get")

	if err == nil {
		t.Fatal("a command ran before the terms were accepted")
	}
	mustContain(t, "error", err.Error(), "https://portal.example")
	if h.api.tenantCalls != 0 {
		t.Fatal("the command reached the API before the terms were accepted")
	}
}

func TestCommand_ConsentStatusUnreadable_RunsAnyway(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.api.consentStatusCode = http.StatusInternalServerError

	err := h.run("tenant", "get")

	if err != nil {
		t.Fatalf("an unreadable consent status blocked the command: %v", err)
	}
	if h.api.tenantCalls != 1 {
		t.Fatalf("tenant calls = %d, want 1", h.api.tenantCalls)
	}
}

func TestAuthLogin_TermsNotAccepted_SaysWhereToAcceptThem(t *testing.T) {
	h := newHarness(t)
	h.api.mustAcceptTerms = true

	err := h.run("auth", "login")

	if err != nil {
		t.Fatalf("login failed: %v", err)
	}
	mustContain(t, "stderr", h.stderr.String(), "https://portal.example")
}

// US-CLI-03

func TestTenantGet_PrintsTheTenantAsATable(t *testing.T) {
	h := newHarness(t)
	h.login()

	err := h.run("tenant", "get")

	if err != nil {
		t.Fatalf("tenant get failed: %v", err)
	}
	out := h.stdout.String()
	mustContain(t, "stdout", out, "7d0c2f9e-4b1a-4c55-9a1e-3f2b8c6d1e01")
	mustContain(t, "stdout", out, "acme")
	mustContain(t, "stdout", out, "Active")
	mustContain(t, "stdout", out, "Max VMs")
	mustContain(t, "stdout", out, "2026-09-01")
	if h.api.authorizationsSeen[len(h.api.authorizationsSeen)-1] == "" {
		t.Fatal("the API call carried no token")
	}
}

func TestTenantGet_JsonOutput_IsTheApiObjectWithExactNumbers(t *testing.T) {
	h := newHarness(t)
	h.api.tenantBody = strings.Replace(acmeTenant, `"debtCeiling":500000`, `"debtCeiling":123456789012`, 1)
	h.login()

	err := h.run("tenant", "get", "-o", "json")

	if err != nil {
		t.Fatalf("tenant get failed: %v", err)
	}
	var printed map[string]json.RawMessage
	decodeErr := json.Unmarshal(h.stdout.Bytes(), &printed)
	if decodeErr != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", decodeErr, h.stdout.String())
	}
	if string(printed["name"]) != `"acme"` {
		t.Fatalf("name = %s", printed["name"])
	}
	if string(printed["debtCeiling"]) != "123456789012" {
		t.Fatalf("debtCeiling = %s, want the exact digits the API sent", printed["debtCeiling"])
	}
}

func TestTenantGet_KnownErrorCode_IsExplainedWithTheCode(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.api.tenantStatusCode = http.StatusForbidden
	// The title says nothing useful on purpose: the sentence must come from the CLI, not from the server.
	h.api.tenantBody = `{"status":403,"title":"Forbidden","code":"NOT_A_MEMBER"}`

	err := h.run("tenant", "get")

	if err == nil {
		t.Fatal("a 403 was reported as success")
	}
	mustContain(t, "error", err.Error(), "not a member of any tenant")
	mustContain(t, "error", err.Error(), "NOT_A_MEMBER")
}

func TestTenantGet_ApiRejectsTheToken_SaysToLogInAgain(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.api.tenantStatusCode = http.StatusUnauthorized
	h.api.tenantBody = ""

	err := h.run("tenant", "get")

	if err == nil {
		t.Fatal("a 401 was reported as success")
	}
	mustContain(t, "error", err.Error(), "openvicloud auth login")
}

func TestTenantGet_UnknownErrorCode_ShowsTheServerTitleAndCode(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.api.tenantStatusCode = http.StatusConflict
	h.api.tenantBody = `{"status":409,"title":"Something new happened","code":"BRAND_NEW_CODE"}`

	err := h.run("tenant", "get")

	if err == nil {
		t.Fatal("a 409 was reported as success")
	}
	mustContain(t, "error", err.Error(), "Something new happened")
	mustContain(t, "error", err.Error(), "BRAND_NEW_CODE")
}

func TestTenantGet_NotLoggedIn_SaysToRunAuthLogin(t *testing.T) {
	h := newHarness(t)

	err := h.run("tenant", "get")

	if err == nil {
		t.Fatal("tenant get worked without a session")
	}
	mustContain(t, "error", err.Error(), "openvicloud auth login")
}

// US-CLI-04

func TestVersion_PrintsTheVersionAndTheEndpoints(t *testing.T) {
	h := newHarness(t)

	err := h.run("version")

	if err != nil {
		t.Fatalf("version failed: %v", err)
	}
	mustContain(t, "stdout", h.stdout.String(), "0.1.0-test")
	mustContain(t, "stdout", h.stdout.String(), h.api.server.URL)
	mustContain(t, "stdout", h.stdout.String(), h.keycloak.server.URL+"/realms/titan")
}

func TestOutputFormat_Unknown_IsRefused(t *testing.T) {
	h := newHarness(t)

	err := h.run("version", "-o", "yaml")

	if err == nil {
		t.Fatal("accepted -o yaml")
	}
}

func TestEndpointsFromEnv_DefaultIsProduction_EnvOverrides(t *testing.T) {
	defaults := command.EndpointsFromEnv(func(string) string { return "" })
	overridden := command.EndpointsFromEnv(func(name string) string {
		values := map[string]string{
			"OPENVICLOUD_API_URL":    "http://api.localhost:8080",
			"OPENVICLOUD_AUTH_URL":   "http://auth.localhost:8080/auth/realms/titan",
			"OPENVICLOUD_PORTAL_URL": "http://portal.localhost:8080",
		}
		return values[name]
	})

	if defaults.Api != "https://api.openvicloud.com" {
		t.Fatalf("default API = %q", defaults.Api)
	}
	if defaults.Auth != "https://auth.openvicloud.com/auth/realms/titan" {
		t.Fatalf("default auth = %q", defaults.Auth)
	}
	if defaults.Portal != "https://portal.openvicloud.com" {
		t.Fatalf("default portal = %q", defaults.Portal)
	}
	if overridden.Api != "http://api.localhost:8080" || overridden.Auth != "http://auth.localhost:8080/auth/realms/titan" || overridden.Portal != "http://portal.localhost:8080" {
		t.Fatalf("overridden = %+v", overridden)
	}
}

// US-SSH-5

func TestVmSsh_RunsTheSystemSsh_WithTheTunnelAsProxyCommand(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.deps.Executable = func() (string, error) { return "/opt/open vicloud/openvicloud", nil }
	var ran []string
	h.deps.RunSSH = func(ctx context.Context, args []string) error {
		ran = args
		return nil
	}

	err := h.run("vm", "ssh", "web-1", "--", "-i", "/home/me/.ssh/id_ed25519")

	if err != nil {
		t.Fatalf("vm ssh failed: %v", err)
	}
	// The destination comes before the extra arguments: ssh still reads options after it, and the first word that is
	// not an option starts a remote command.
	want := []string{
		"-o", "ProxyCommand='/opt/open vicloud/openvicloud' vm tunnel 5b0e7f4c-0000-4000-8000-000000000001",
		"-o", "HostKeyAlias=openvicloud-5b0e7f4c-0000-4000-8000-000000000001",
		"ubuntu@web-1",
		"-i", "/home/me/.ssh/id_ed25519",
	}
	if strings.Join(ran, "|") != strings.Join(want, "|") {
		t.Fatalf("ssh args:\n got %q\nwant %q", ran, want)
	}
}

func TestVmSsh_ById_AndAnotherUser(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.deps.Executable = func() (string, error) { return "/usr/local/bin/openvicloud", nil }
	var ran []string
	h.deps.RunSSH = func(ctx context.Context, args []string) error {
		ran = args
		return nil
	}

	err := h.run("vm", "ssh", "--user", "admin", "5b0e7f4c-0000-4000-8000-000000000002")

	if err != nil {
		t.Fatalf("vm ssh failed: %v", err)
	}
	if ran[4] != "admin@db-1" {
		t.Fatalf("destination: %q", ran)
	}
}

func TestVmSsh_UnknownVm_SaysSo_AndRunsNoSsh(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.deps.Executable = func() (string, error) { return "/usr/local/bin/openvicloud", nil }
	ran := false
	h.deps.RunSSH = func(ctx context.Context, args []string) error {
		ran = true
		return nil
	}

	err := h.run("vm", "ssh", "nope")

	if err == nil {
		t.Fatal("vm ssh of an unknown VM succeeded")
	}
	mustContain(t, "error", err.Error(), `no VM named "nope"`)
	if ran {
		t.Fatal("ssh ran for an unknown VM")
	}
}

func TestVmSsh_SshExitCode_IsPassedOn(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.deps.Executable = func() (string, error) { return "/usr/local/bin/openvicloud", nil }
	h.deps.RunSSH = func(ctx context.Context, args []string) error { return &command.ExitCode{Code: 255} }

	err := h.run("vm", "ssh", "web-1")

	var exitCode *command.ExitCode
	if !errors.As(err, &exitCode) || exitCode.Code != 255 {
		t.Fatalf("want exit code 255, got %v", err)
	}
}

func TestVmTunnel_CarriesBytesBothWays_WithTheCustomersToken(t *testing.T) {
	h := newHarness(t)
	h.login()
	in, feed := io.Pipe()
	defer feed.Close()
	go feed.Write([]byte("ping"))
	var out bytes.Buffer

	err := vm.Tunnel(context.Background(), h.deps, "5b0e7f4c-0000-4000-8000-000000000001", in, &out)

	if err != nil {
		t.Fatalf("tunnel failed: %v", err)
	}
	if out.String() != sshBanner+"ping" {
		t.Fatalf("tunnel output %q", out.String())
	}
	if !strings.HasPrefix(h.api.sshAuthorization, "Bearer e30.") {
		t.Fatalf("authorization sent: %q", h.api.sshAuthorization)
	}
	if h.api.sshVmId != "5b0e7f4c-0000-4000-8000-000000000001" {
		t.Fatalf("vm id sent: %q", h.api.sshVmId)
	}
}

func TestVmTunnel_RefusedSession_ExplainsTheCode(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.api.sshRefusal = `{"status":422,"title":"The VM is still starting; try again in a minute.","code":"VM_AGENT_NOT_READY"}`
	var out bytes.Buffer

	err := vm.Tunnel(context.Background(), h.deps, "5b0e7f4c-0000-4000-8000-000000000001", strings.NewReader(""), &out)

	if err == nil {
		t.Fatal("a refused session succeeded")
	}
	mustContain(t, "error", err.Error(), "try again in a minute")
	mustContain(t, "error", err.Error(), "VM_AGENT_NOT_READY")
	if out.Len() != 0 {
		t.Fatalf("a refusal wrote to ssh's stream: %q", out.String())
	}
}

func TestVmTunnel_NotLoggedIn_SaysToRunAuthLogin(t *testing.T) {
	h := newHarness(t)
	var out bytes.Buffer

	err := vm.Tunnel(context.Background(), h.deps, "5b0e7f4c-0000-4000-8000-000000000001", strings.NewReader(""), &out)

	if err == nil {
		t.Fatal("tunnel without a login succeeded")
	}
	mustContain(t, "error", err.Error(), "openvicloud auth login")
}

// ssh ends its ProxyCommand with a signal when the session is over: the tunnel then closes the websocket normally, so
// the control plane ends the command in the guest at once.
func TestVmTunnel_Stopped_ClosesTheSessionNormally(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.api.sshHold = true
	h.api.sshCloseStatus = make(chan websocket.StatusCode, 1)
	in, feed := io.Pipe()
	defer feed.Close()
	out, received := io.Pipe()
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() {
		done <- vm.Tunnel(ctx, h.deps, "5b0e7f4c-0000-4000-8000-000000000001", in, received)
	}()
	// Stop only once the tunnel holds the session: the banner has come through it.
	banner := make([]byte, len(sshBanner))
	if _, err := io.ReadFull(out, banner); err != nil {
		t.Fatalf("reading the banner: %v", err)
	}
	go io.Copy(io.Discard, out)
	stop()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("tunnel: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("tunnel did not return after it was stopped")
	}
	select {
	case status := <-h.api.sshCloseStatus:
		if status != websocket.StatusNormalClosure {
			t.Fatalf("close status %v, want normal closure", status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server saw no close")
	}
}

// An ssh killed by a signal has no exit status; the CLI then exits 255, as ssh does for its own errors.
func TestExitCodeOf_ASignalledSsh_Is255(t *testing.T) {
	if command.ExitCodeOf(-1) != 255 {
		t.Fatalf("exit code of a signalled ssh: %d", command.ExitCodeOf(-1))
	}
	if command.ExitCodeOf(3) != 3 {
		t.Fatalf("exit code 3 became %d", command.ExitCodeOf(3))
	}
}
