package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// US-CLI-10 and US-CLI-16: catalog, VM lifecycle with --wait, SSH keys — against the fake /v1 API of main_test.go.

const vmJson = `{"id":"%s","name":"web-2","clusterId":"local-mac","currentState":%s,"desiredState":"%s","currentExistence":"%s",` +
	`"desiredExistence":"%s","transition":%s,"ipAddress":%s,"requestFailureReason":%s}`

// vmRead is one answer of GET /v1/tenant/vms/{id} for the VM the create makes.
func vmRead(currentState string, desiredState string, currentExistence string, desiredExistence string, transition string, ip string, failure string) string {
	return sprintf(vmJson, createdVmId, currentState, desiredState, currentExistence, desiredExistence, transition, ip, failure)
}

func sprintf(format string, args ...string) string {
	out := format
	for _, arg := range args {
		out = strings.Replace(out, "%s", arg, 1)
	}
	return out
}

func lastWrite(t *testing.T, h *harness) recordedWrite {
	t.Helper()
	if len(h.api.writes) == 0 {
		t.Fatalf("the API received no write")
	}
	return h.api.writes[len(h.api.writes)-1]
}

// US-CLI-10 AC-1

func TestClusterList_PrintsIdAndName(t *testing.T) {
	h := newHarness(t)
	h.login()

	err := h.run("cluster", "list")

	if err != nil {
		t.Fatalf("cluster list: %v", err)
	}
	mustContain(t, "stdout", h.stdout.String(), "ID")
	mustContain(t, "stdout", h.stdout.String(), "local-mac")
	mustContain(t, "stdout", h.stdout.String(), "Local Mac")
}

func TestTemplateList_ByCluster_AsksForThatCluster_AndPrintsOsAndDisk(t *testing.T) {
	h := newHarness(t)
	h.login()

	err := h.run("template", "list", "--cluster", "local-mac")

	if err != nil {
		t.Fatalf("template list: %v", err)
	}
	if len(h.api.templateClusters) != 1 || h.api.templateClusters[0] != "local-mac" {
		t.Fatalf("clusterId sent = %v", h.api.templateClusters)
	}
	mustContain(t, "stdout", h.stdout.String(), "Ubuntu-22.04-Standard")
	mustContain(t, "stdout", h.stdout.String(), "Ubuntu 22.04")
	mustContain(t, "stdout", h.stdout.String(), "10")
}

func TestSizeList_KeepsEveryDigitOfThePrice(t *testing.T) {
	h := newHarness(t)
	h.login()

	err := h.run("size", "list")
	if err != nil {
		t.Fatalf("size list: %v", err)
	}
	mustContain(t, "table", h.stdout.String(), "0.0123456789")
	mustContain(t, "table", h.stdout.String(), "4096")

	err = h.run("size", "list", "-o", "json")
	if err != nil {
		t.Fatalf("size list -o json: %v", err)
	}
	mustContain(t, "json", h.stdout.String(), `"pricePerHour": 0.0123456789`)
}

// US-CLI-10 AC-2

func TestVmList_PrintsEachVmWithItsStateAndAddress(t *testing.T) {
	h := newHarness(t)
	h.login()

	err := h.run("vm", "list")

	if err != nil {
		t.Fatalf("vm list: %v", err)
	}
	out := h.stdout.String()
	mustContain(t, "stdout", out, "web-1")
	mustContain(t, "stdout", out, "5b0e7f4c-0000-4000-8000-000000000001")
	mustContain(t, "stdout", out, "Running")
	mustContain(t, "stdout", out, "10.0.0.2")
	mustContain(t, "stdout", out, "db-1")
	mustContain(t, "stdout", out, "Stopped")
}

func TestVmGet_ByNameAndById_PrintsTheVm(t *testing.T) {
	h := newHarness(t)
	h.login()

	err := h.run("vm", "get", "web-1")
	if err != nil {
		t.Fatalf("vm get by name: %v", err)
	}
	mustContain(t, "by name", h.stdout.String(), "5b0e7f4c-0000-4000-8000-000000000001")
	mustContain(t, "by name", h.stdout.String(), "Running")

	err = h.run("vm", "get", "5b0e7f4c-0000-4000-8000-000000000002", "-o", "json")
	if err != nil {
		t.Fatalf("vm get by id: %v", err)
	}
	mustContain(t, "by id", h.stdout.String(), `"name": "db-1"`)
}

func TestVmGet_UnknownVm_SaysSo(t *testing.T) {
	h := newHarness(t)
	h.login()

	err := h.run("vm", "get", "nope")

	if err == nil || !strings.Contains(err.Error(), `no VM named "nope"`) {
		t.Fatalf("err = %v", err)
	}
}

// US-CLI-10 AC-3, US-CLI-16 AC-2

func TestVmCreate_SendsTheIdsOfTheNamedResources_AndPrintsTheNewId(t *testing.T) {
	h := newHarness(t)
	h.login()

	err := h.run("vm", "create", "web-2", "--cluster", "local-mac", "--network", "app-net",
		"--template", "Ubuntu-22.04-Standard", "--size", "Medium", "--ssh-key", "laptop")

	if err != nil {
		t.Fatalf("vm create: %v", err)
	}
	write := lastWrite(t, h)
	if write.Method != "POST" || write.Path != "/v1/tenant/vms" || write.IdempotencyKey == "" {
		t.Fatalf("write = %+v", write)
	}
	want := map[string]any{
		"name": "web-2", "clusterId": "local-mac", "networkId": "9a1d0000-0000-4000-8000-000000000001",
		"templateId": "tpl-ubuntu", "vmOfferingId": "m",
		"sshKeyIds": []any{"c3c30000-0000-4000-8000-000000000001"},
	}
	for key, value := range want {
		got, _ := json.Marshal(write.Body[key])
		expected, _ := json.Marshal(value)
		if string(got) != string(expected) {
			t.Fatalf("body[%s] = %s, want %s (body %v)", key, got, expected, write.Body)
		}
	}
	mustContain(t, "stdout", h.stdout.String(), createdVmId)
}

func TestVmCreate_UnknownNetwork_SaysSo_AndCreatesNothing(t *testing.T) {
	h := newHarness(t)
	h.login()

	err := h.run("vm", "create", "web-2", "--cluster", "local-mac", "--network", "nope",
		"--template", "tpl-ubuntu", "--size", "m")

	if err == nil || !strings.Contains(err.Error(), `no network named "nope"`) {
		t.Fatalf("err = %v", err)
	}
	if len(h.api.writes) != 0 {
		t.Fatalf("writes = %+v", h.api.writes)
	}
}

func TestVmCreate_UnknownSshKey_SaysSo_AndCreatesNothing(t *testing.T) {
	h := newHarness(t)
	h.login()

	err := h.run("vm", "create", "web-2", "--cluster", "local-mac", "--network", "app-net",
		"--template", "tpl-ubuntu", "--size", "m", "--ssh-key", "laptop", "--ssh-key", "desktop")

	if err == nil || !strings.Contains(err.Error(), `no SSH key named "desktop"`) {
		t.Fatalf("err = %v", err)
	}
	if len(h.api.writes) != 0 {
		t.Fatalf("writes = %+v", h.api.writes)
	}
}

// US-CLI-10 AC-4

func TestVmCreate_AnswerLost_SendsAgainWithTheSameKey(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.api.createAnswers = []int{502}

	err := h.run("vm", "create", "web-2", "--cluster", "local-mac", "--network", "app-net",
		"--template", "tpl-ubuntu", "--size", "m")

	if err != nil {
		t.Fatalf("vm create: %v", err)
	}
	if len(h.api.writes) != 2 {
		t.Fatalf("writes = %d, want 2", len(h.api.writes))
	}
	first, second := h.api.writes[0].IdempotencyKey, h.api.writes[1].IdempotencyKey
	if first == "" || first != second {
		t.Fatalf("keys = %q, %q: want the same key on both attempts", first, second)
	}
	mustContain(t, "stdout", h.stdout.String(), createdVmId)
}

func TestVmCreate_AnswerLostEveryTime_GivesUpAfterThreeSends(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.api.createAnswers = []int{502, 503, 504, 502}

	err := h.run("vm", "create", "web-2", "--cluster", "local-mac", "--network", "app-net", "--template", "tpl-ubuntu", "--size", "m")

	if err == nil {
		t.Fatalf("vm create succeeded, want the last refusal")
	}
	if len(h.api.writes) != 3 {
		t.Fatalf("sends = %d, want 3", len(h.api.writes))
	}
}

func TestVmCreate_TwoRuns_UseTwoKeys(t *testing.T) {
	h := newHarness(t)
	h.login()

	for i := 0; i < 2; i++ {
		err := h.run("vm", "create", "web-2", "--cluster", "local-mac", "--network", "app-net", "--template", "tpl-ubuntu", "--size", "m")
		if err != nil {
			t.Fatalf("vm create: %v", err)
		}
	}

	if h.api.writes[0].IdempotencyKey == h.api.writes[1].IdempotencyKey {
		t.Fatalf("two runs sent the same key %q", h.api.writes[0].IdempotencyKey)
	}
}

// US-CLI-10 AC-6

func TestVmCreate_Wait_UntilRunning_PrintsTheFinalState(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.api.vmReads = map[string][]string{createdVmId: {
		vmRead("null", "Running", "NotCreated", "Exists", `"Creating"`, "null", "null"),
		vmRead("null", "Running", "Exists", "Exists", `"Creating"`, "null", "null"),
		vmRead(`"Running"`, "Running", "Exists", "Exists", "null", `"10.0.0.9"`, "null"),
	}}

	err := h.run("vm", "create", "web-2", "--cluster", "local-mac", "--network", "app-net",
		"--template", "tpl-ubuntu", "--size", "m", "--wait")

	if err != nil {
		t.Fatalf("vm create --wait: %v", err)
	}
	mustContain(t, "stdout", h.stdout.String(), "Running")
	mustContain(t, "stdout", h.stdout.String(), "10.0.0.9")
}

func TestVmCreate_Wait_RequestFailed_FailsWithTheReason(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.api.vmReads = map[string][]string{createdVmId: {
		vmRead("null", "Running", "NotCreated", "Exists", `"Creating"`, "null", "null"),
		vmRead("null", "Running", "NotCreated", "Exists", "null", "null", `"VM provisioning failed due to a system error."`),
	}}

	err := h.run("vm", "create", "web-2", "--cluster", "local-mac", "--network", "app-net",
		"--template", "tpl-ubuntu", "--size", "m", "--wait")

	if err == nil || !strings.Contains(err.Error(), "VM provisioning failed due to a system error.") {
		t.Fatalf("err = %v", err)
	}
}

func TestVmStop_Wait_RequestGaveUp_FailsWithTheState(t *testing.T) {
	h := newHarness(t)
	h.login()
	web1 := "5b0e7f4c-0000-4000-8000-000000000001"
	h.api.vmReads = map[string][]string{web1: {
		strings.Replace(vmRead(`"Running"`, "Stopped", "Exists", "Exists", `"Stopping"`, "null", "null"), createdVmId, web1, 1),
		strings.Replace(vmRead(`"Running"`, "Running", "Exists", "Exists", "null", "null", "null"), createdVmId, web1, 1),
	}}

	err := h.run("vm", "stop", "web-1", "--wait")

	if err == nil || !strings.Contains(err.Error(), "Running") || !strings.Contains(err.Error(), "Stopped") {
		t.Fatalf("err = %v", err)
	}
}

func TestVmDelete_Wait_UntilDeleted(t *testing.T) {
	h := newHarness(t)
	h.login()
	web1 := "5b0e7f4c-0000-4000-8000-000000000001"
	h.api.vmReads = map[string][]string{web1: {
		strings.Replace(vmRead(`"Running"`, "Running", "Exists", "Deleted", `"Deleting"`, "null", "null"), createdVmId, web1, 1),
		strings.Replace(vmRead("null", "Running", "Deleted", "Deleted", "null", "null", "null"), createdVmId, web1, 1),
	}}

	err := h.run("vm", "delete", "web-1", "--yes", "--wait")

	if err != nil {
		t.Fatalf("vm delete --wait: %v", err)
	}
	mustContain(t, "stderr", h.stderr.String(), "deleted")
}

func TestVmStart_Wait_NeverDone_StopsAtTheTimeout(t *testing.T) {
	h := newHarness(t)
	h.login()
	db1 := "5b0e7f4c-0000-4000-8000-000000000002"
	h.api.vmReads = map[string][]string{db1: {
		strings.Replace(vmRead(`"Stopped"`, "Running", "Exists", "Exists", `"Starting"`, "null", "null"), createdVmId, db1, 1),
	}}
	started := h.now

	err := h.run("vm", "start", "db-1", "--wait", "--timeout", "1m")

	if err == nil || !strings.Contains(err.Error(), "still Starting") {
		t.Fatalf("err = %v", err)
	}
	if h.now.Sub(started) < 60e9 {
		t.Fatalf("waited %v, want at least the timeout", h.now.Sub(started))
	}
}

// US-CLI-10 AC-5

func TestVmStartStopRename_SendTheRequestToTheVmNamed(t *testing.T) {
	h := newHarness(t)
	h.login()

	steps := []struct {
		args   []string
		method string
		path   string
	}{
		{[]string{"vm", "start", "db-1"}, "POST", "/v1/tenant/vms/5b0e7f4c-0000-4000-8000-000000000002/start"},
		{[]string{"vm", "stop", "web-1"}, "POST", "/v1/tenant/vms/5b0e7f4c-0000-4000-8000-000000000001/stop"},
		{[]string{"vm", "rename", "web-1", "web-01"}, "PATCH", "/v1/tenant/vms/5b0e7f4c-0000-4000-8000-000000000001"},
	}
	for _, step := range steps {
		err := h.run(step.args...)
		if err != nil {
			t.Fatalf("%v: %v", step.args, err)
		}
		write := lastWrite(t, h)
		if write.Method != step.method || write.Path != step.path {
			t.Fatalf("%v sent %s %s", step.args, write.Method, write.Path)
		}
	}
	if lastWrite(t, h).Body["name"] != "web-01" {
		t.Fatalf("rename body = %v", lastWrite(t, h).Body)
	}
}

func TestVmDelete_AnswerIsNotYes_DeletesNothing(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.deps.Stdin = strings.NewReader("n\n")

	err := h.run("vm", "delete", "web-1")

	if err == nil || !strings.Contains(err.Error(), "not deleted") {
		t.Fatalf("err = %v", err)
	}
	mustContain(t, "stderr", h.stderr.String(), "Delete VM web-1")
	if len(h.api.writes) != 0 {
		t.Fatalf("writes = %+v", h.api.writes)
	}
}

func TestVmDelete_AnswerYes_Deletes(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.deps.Stdin = strings.NewReader("y\n")

	err := h.run("vm", "delete", "web-1")

	if err != nil {
		t.Fatalf("vm delete: %v", err)
	}
	write := lastWrite(t, h)
	if write.Method != "DELETE" || write.Path != "/v1/tenant/vms/5b0e7f4c-0000-4000-8000-000000000001" {
		t.Fatalf("write = %+v", write)
	}
}

// US-CLI-10 AC-7

func TestVmCreate_ServerRefuses_ShowsTheTitleAndTheCode(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.api.writeRefusal = 422
	h.api.writeRefusalBody = `{"status":422,"title":"VM quota exceeded (max 5).","code":"VM_QUOTA_EXCEEDED"}`

	err := h.run("vm", "create", "web-2", "--cluster", "local-mac", "--network", "app-net", "--template", "tpl-ubuntu", "--size", "m")

	if err == nil || !strings.Contains(err.Error(), "VM quota exceeded (max 5). (VM_QUOTA_EXCEEDED)") {
		t.Fatalf("err = %v", err)
	}
}

// US-CLI-16 AC-1, AC-3

func TestSshKeyAdd_SendsTheFile_AndPrintsTheId(t *testing.T) {
	h := newHarness(t)
	h.login()
	file := filepath.Join(t.TempDir(), "id_ed25519.pub")
	os.WriteFile(file, []byte("ssh-ed25519 AAAAC3Nza laptop@home\n"), 0o600)

	err := h.run("ssh-key", "add", "desktop", "--public-key-file", file)

	if err != nil {
		t.Fatalf("ssh-key add: %v", err)
	}
	write := lastWrite(t, h)
	if write.Path != "/v1/tenant/ssh-keys" || write.IdempotencyKey == "" || write.Body["name"] != "desktop" ||
		write.Body["publicKey"] != "ssh-ed25519 AAAAC3Nza laptop@home" {
		t.Fatalf("write = %+v", write)
	}
	mustContain(t, "stdout", h.stdout.String(), "c3c30000-0000-4000-8000-0000000000bb")
}

func TestSshKeyAdd_FileUnreadable_SaysWhichFile_AndCallsNoApi(t *testing.T) {
	h := newHarness(t)
	h.login()

	err := h.run("ssh-key", "add", "desktop", "--public-key-file", "/no/such/key.pub")

	if err == nil || !strings.Contains(err.Error(), "/no/such/key.pub") {
		t.Fatalf("err = %v", err)
	}
	if len(h.api.writes) != 0 {
		t.Fatalf("writes = %+v", h.api.writes)
	}
}

func TestSshKeyAdd_KeyExists_ShowsTheTitleAndTheCode(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.api.writeRefusal = 409
	h.api.writeRefusalBody = `{"status":409,"title":"This key is already in your tenant.","code":"SSH_KEY_EXISTS"}`
	file := filepath.Join(t.TempDir(), "id_ed25519.pub")
	os.WriteFile(file, []byte("ssh-ed25519 AAAAC3Nza laptop@home\n"), 0o600)

	err := h.run("ssh-key", "add", "laptop-2", "--public-key-file", file)

	if err == nil || !strings.Contains(err.Error(), "(SSH_KEY_EXISTS)") {
		t.Fatalf("err = %v", err)
	}
}

func TestSshKeyList_PrintsNameIdTypeAndFingerprint(t *testing.T) {
	h := newHarness(t)
	h.login()

	err := h.run("ssh-key", "list")

	if err != nil {
		t.Fatalf("ssh-key list: %v", err)
	}
	for _, want := range []string{"laptop", "c3c30000-0000-4000-8000-000000000001", "ssh-ed25519", "SHA256:abc"} {
		mustContain(t, "stdout", h.stdout.String(), want)
	}
}

func TestSshKeyDelete_ByName_AfterConfirming(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.deps.Stdin = strings.NewReader("yes\n")

	err := h.run("ssh-key", "delete", "laptop")

	if err != nil {
		t.Fatalf("ssh-key delete: %v", err)
	}
	write := lastWrite(t, h)
	if write.Method != "DELETE" || write.Path != "/v1/tenant/ssh-keys/c3c30000-0000-4000-8000-000000000001" {
		t.Fatalf("write = %+v", write)
	}
}
