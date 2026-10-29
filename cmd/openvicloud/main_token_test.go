package main

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenViCloud/titan-cli/command"
	"github.com/OpenViCloud/titan-cli/command/vm"
)

// An API token from OPENVICLOUD_TOKEN replaces the browser login (titan-ops features/cli/design.md#api-token).

const apiToken = "ovc_q3v9Xk2LmP0aZ8yT4rWbN6cJ1dF5gH7sE9uIoK2pQwA"

func TestApiToken_CommandRunsWithoutALogin_AndSendsTheToken(t *testing.T) {
	h := newHarness(t)
	h.deps.ApiToken = apiToken

	err := h.run("tenant", "get")

	if err != nil {
		t.Fatalf("tenant get with a token failed: %v", err)
	}
	if len(h.api.authorizationsSeen) == 0 {
		t.Fatal("no request reached the API")
	}
	for _, seen := range h.api.authorizationsSeen {
		if seen != "Bearer "+apiToken {
			t.Fatalf("a request carried %q, not the token", seen)
		}
	}
}

func TestApiToken_WinsOverASavedSession(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.api.authorizationsSeen = nil
	h.deps.ApiToken = apiToken

	err := h.run("tenant", "get")

	if err != nil {
		t.Fatalf("tenant get failed: %v", err)
	}
	for _, seen := range h.api.authorizationsSeen {
		if seen != "Bearer "+apiToken {
			t.Fatalf("a request carried %q, not the token", seen)
		}
	}
}

func TestApiToken_Refused_SaysTheTokenWasRefused_NotToLogIn(t *testing.T) {
	h := newHarness(t)
	h.deps.ApiToken = apiToken
	h.api.tenantStatusCode = http.StatusUnauthorized
	h.api.tenantBody = ""

	err := h.run("tenant", "get")

	if err == nil {
		t.Fatal("a refused token succeeded")
	}
	mustContain(t, "error", err.Error(), "OPENVICLOUD_TOKEN")
	if strings.Contains(err.Error(), "auth login") {
		t.Fatalf("told a token user to log in: %s", err)
	}
}

func TestApiToken_VmTunnel_SendsTheToken(t *testing.T) {
	h := newHarness(t)
	h.deps.ApiToken = apiToken
	h.api.sshRefusal = `{"status":422,"title":"The VM is still starting; try again in a minute.","code":"VM_AGENT_NOT_READY"}`
	var out bytes.Buffer

	_ = vm.Tunnel(context.Background(), h.deps, "5b0e7f4c-0000-4000-8000-000000000001", strings.NewReader(""), &out)

	if h.api.sshAuthorization != "Bearer "+apiToken {
		t.Fatalf("the ssh upgrade carried %q, not the token", h.api.sshAuthorization)
	}
}

func TestApiTokenFromEnv_TrimsTheValue_AndIsEmptyWhenUnset(t *testing.T) {
	env := map[string]string{"OPENVICLOUD_TOKEN": "  " + apiToken + "\n"}
	if got := command.ApiTokenFromEnv(func(key string) string { return env[key] }); got != apiToken {
		t.Fatalf("token = %q", got)
	}
	if got := command.ApiTokenFromEnv(func(string) string { return "" }); got != "" {
		t.Fatalf("token without the variable = %q", got)
	}
}
