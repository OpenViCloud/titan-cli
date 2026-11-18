package main

import (
	"strings"
	"testing"
)

// titan-ops features/vm/design.md#vm-key-addition: `vm add-ssh-key` against the fake /v1 API of main_test.go.

const keyAdditionJson = `{"id":"%s","status":"%s","sshKeyIds":["c3c30000-0000-4000-8000-000000000001"],"restartNeeded":%s,"requestFailureReason":%s}`

// keyAdditionRead is one answer of GET .../key-additions/{id} for the addition the POST makes.
func keyAdditionRead(status string, restartNeeded string, failure string) string {
	return sprintf(keyAdditionJson, createdKeyAdditionId, status, restartNeeded, failure)
}

func TestVmAddSshKey_SendsTheResolvedId_WaitsForAdded_PrintsTheResult(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.api.keyAdditionReads = map[string][]string{createdKeyAdditionId: {
		keyAdditionRead("Adding", "false", "null"),
		keyAdditionRead("Added", "false", "null"),
	}}

	err := h.run("vm", "add-ssh-key", "web-1", "--ssh-key", "laptop")

	if err != nil {
		t.Fatalf("vm add-ssh-key: %v", err)
	}
	write := lastWrite(t, h)
	if write.Method != "POST" || write.Path != "/v1/tenant/vms/5b0e7f4c-0000-4000-8000-000000000001/key-additions" || write.IdempotencyKey == "" {
		t.Fatalf("write = %+v", write)
	}
	wantIds, _ := write.Body["sshKeyIds"].([]any)
	if len(wantIds) != 1 || wantIds[0] != "c3c30000-0000-4000-8000-000000000001" {
		t.Fatalf("sshKeyIds = %v", write.Body["sshKeyIds"])
	}
	out := h.stdout.String()
	mustContain(t, "stdout", out, createdKeyAdditionId)
	mustContain(t, "stdout", out, "Added")
	mustContain(t, "stdout", out, "Restart needed")
	mustContain(t, "stdout", out, "false")
}

func TestVmAddSshKey_Wait_Added_WithRestartNeeded_PrintsARestartLine(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.api.keyAdditionReads = map[string][]string{createdKeyAdditionId: {keyAdditionRead("Added", "true", "null")}}

	err := h.run("vm", "add-ssh-key", "web-1", "--ssh-key", "laptop")

	if err != nil {
		t.Fatalf("vm add-ssh-key: %v", err)
	}
	mustContain(t, "stdout", h.stdout.String(), "true")
	mustContain(t, "stderr", h.stderr.String(), "Stop VM web-1 and start it again")
}

func TestVmAddSshKey_Wait_Added_NoRestartNeeded_PrintsNoRestartLine(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.api.keyAdditionReads = map[string][]string{createdKeyAdditionId: {keyAdditionRead("Added", "false", "null")}}

	err := h.run("vm", "add-ssh-key", "web-1", "--ssh-key", "laptop")

	if err != nil {
		t.Fatalf("vm add-ssh-key: %v", err)
	}
	if strings.Contains(h.stderr.String(), "Restart") {
		t.Fatalf("stderr mentions a restart although none is needed: %s", h.stderr.String())
	}
}

func TestVmAddSshKey_Wait_Failed_FailsWithTheReason(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.api.keyAdditionReads = map[string][]string{createdKeyAdditionId: {
		keyAdditionRead("Adding", "false", "null"),
		keyAdditionRead("Failed", "false", `"the agent on the VM is unreachable"`),
	}}

	err := h.run("vm", "add-ssh-key", "web-1", "--ssh-key", "laptop")

	if err == nil || !strings.Contains(err.Error(), "the agent on the VM is unreachable") {
		t.Fatalf("err = %v", err)
	}
}

func TestVmAddSshKey_NoWait_PrintsTheId_AndPollsNothing(t *testing.T) {
	h := newHarness(t)
	h.login()

	err := h.run("vm", "add-ssh-key", "web-1", "--ssh-key", "laptop", "--no-wait")

	if err != nil {
		t.Fatalf("vm add-ssh-key --no-wait: %v", err)
	}
	mustContain(t, "stdout", h.stdout.String(), createdKeyAdditionId)
	if strings.Contains(h.stdout.String(), "Status") {
		t.Fatalf("--no-wait printed a status although it never polled: %s", h.stdout.String())
	}
}

func TestVmAddSshKey_NoSshKeyFlag_SaysSo_AndSendsNoRequest(t *testing.T) {
	h := newHarness(t)
	h.login()
	seen := len(h.api.authorizationsSeen)

	err := h.run("vm", "add-ssh-key", "web-1")

	if err == nil || !strings.Contains(err.Error(), "at least one --ssh-key is required") {
		t.Fatalf("err = %v", err)
	}
	if len(h.api.writes) != 0 {
		t.Fatalf("writes = %+v", h.api.writes)
	}
	if len(h.api.authorizationsSeen) != seen {
		t.Fatalf("the CLI called the API before checking the flags: calls went from %d to %d", seen, len(h.api.authorizationsSeen))
	}
}

func TestVmAddSshKey_MoreThanTenKeys_SaysSo_AndSendsNoRequest(t *testing.T) {
	h := newHarness(t)
	h.login()
	args := []string{"vm", "add-ssh-key", "web-1"}
	for i := 0; i < 11; i++ {
		args = append(args, "--ssh-key", "key-"+strings.Repeat("x", i+1))
	}

	err := h.run(args...)

	if err == nil || !strings.Contains(err.Error(), "at most 10 --ssh-key flags are accepted, got 11") {
		t.Fatalf("err = %v", err)
	}
	if len(h.api.writes) != 0 {
		t.Fatalf("writes = %+v", h.api.writes)
	}
}

// Repeating the same --ssh-key eleven times is one distinct key, so it must not be refused for being over ten.
func TestVmAddSshKey_SameFlagElevenTimes_IsOneKey_SendsOneId(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.api.keyAdditionReads = map[string][]string{createdKeyAdditionId: {keyAdditionRead("Added", "false", "null")}}
	args := []string{"vm", "add-ssh-key", "web-1"}
	for i := 0; i < 11; i++ {
		args = append(args, "--ssh-key", "laptop")
	}

	err := h.run(args...)

	if err != nil {
		t.Fatalf("vm add-ssh-key: %v", err)
	}
	ids, _ := lastWrite(t, h).Body["sshKeyIds"].([]any)
	if len(ids) != 1 {
		t.Fatalf("sshKeyIds = %v, want one id", ids)
	}
}

// The same key given once by name and once by its own id resolves to one id, same as vm create.
func TestVmAddSshKey_NameAndId_OfTheSameKey_SendsOneId(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.api.keyAdditionReads = map[string][]string{createdKeyAdditionId: {keyAdditionRead("Added", "false", "null")}}

	err := h.run("vm", "add-ssh-key", "web-1", "--ssh-key", "laptop", "--ssh-key", "c3c30000-0000-4000-8000-000000000001")

	if err != nil {
		t.Fatalf("vm add-ssh-key: %v", err)
	}
	ids, _ := lastWrite(t, h).Body["sshKeyIds"].([]any)
	if len(ids) != 1 {
		t.Fatalf("sshKeyIds = %v, want one id", ids)
	}
}

func TestVmAddSshKey_UnknownSshKey_SaysSo_AndSendsNoRequest(t *testing.T) {
	h := newHarness(t)
	h.login()

	err := h.run("vm", "add-ssh-key", "web-1", "--ssh-key", "laptop", "--ssh-key", "desktop")

	if err == nil || !strings.Contains(err.Error(), `no SSH key named "desktop"`) {
		t.Fatalf("err = %v", err)
	}
	if len(h.api.writes) != 0 {
		t.Fatalf("writes = %+v", h.api.writes)
	}
}

func TestVmAddSshKey_UnknownVm_SaysSo_AndSendsNoRequest(t *testing.T) {
	h := newHarness(t)
	h.login()

	err := h.run("vm", "add-ssh-key", "nope", "--ssh-key", "laptop")

	if err == nil || !strings.Contains(err.Error(), `no VM named "nope"`) {
		t.Fatalf("err = %v", err)
	}
	if len(h.api.writes) != 0 {
		t.Fatalf("writes = %+v", h.api.writes)
	}
}

// titan-ops features/vm/user-stories.md#us-vm-add-key: the server refuses with 422 when the VM is not Running, or another addition is already
// waiting on it.
func TestVmAddSshKey_VmInvalidStatus_ShowsTheTitleAndTheCode(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.api.writeRefusal = 422
	h.api.writeRefusalBody = `{"status":422,"title":"VM must be Running to add keys.","code":"VM_INVALID_STATUS"}`

	err := h.run("vm", "add-ssh-key", "web-1", "--ssh-key", "laptop")

	if err == nil || !strings.Contains(err.Error(), "VM must be Running to add keys. (VM_INVALID_STATUS)") {
		t.Fatalf("err = %v", err)
	}
}

func TestVmAddSshKey_SshKeyNotFoundOnTheServer_ShowsTheTitleAndTheCode(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.api.writeRefusal = 404
	h.api.writeRefusalBody = `{"status":404,"title":"SSH key not found.","code":"SSH_KEY_NOT_FOUND"}`

	err := h.run("vm", "add-ssh-key", "web-1", "--ssh-key", "laptop")

	if err == nil || !strings.Contains(err.Error(), "SSH_KEY_NOT_FOUND") {
		t.Fatalf("err = %v", err)
	}
}
