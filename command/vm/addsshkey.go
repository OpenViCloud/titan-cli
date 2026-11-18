package vm

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/OpenViCloud/titan-cli/api"
	"github.com/OpenViCloud/titan-cli/command"
	"github.com/OpenViCloud/titan-cli/command/sshkey"
	"github.com/OpenViCloud/titan-cli/output"
	"github.com/OpenViCloud/titan-cli/wait"
)

// A key addition is quick and its only useful outputs (restartNeeded, a failure reason) only exist once it has
// ended, so this command waits by default, unlike create/start/stop/delete (titan-ops features/vm/design.md#vm-key-addition).
func newAddSshKey(deps *command.Deps) *cobra.Command {
	var sshKeys []string
	var noWait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "add-ssh-key <vm name or id> --ssh-key <name|id>",
		Short: "Add saved SSH keys to a running VM",
		Long: "Add one or more saved SSH keys to a VM that is already running. SSH keys are given by name or id, as in\n" +
			"`vm create`: repeat --ssh-key for several keys, 1 to 10, same key given twice counts once.\n" +
			"The VM must be Running, and no other key addition may be waiting on it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			names := dedupeStrings(sshKeys)
			if len(names) == 0 {
				return fmt.Errorf("at least one --ssh-key is required")
			}
			if len(names) > 10 {
				return fmt.Errorf("at most 10 --ssh-key flags are accepted, got %d", len(names))
			}
			client, err := deps.Client(ctx)
			if err != nil {
				return err
			}
			id, name, err := resolve(ctx, client, args[0])
			if err != nil {
				return err
			}
			keyIds, err := resolveSshKeys(ctx, client, names)
			if err != nil {
				return err
			}
			params := &api.TitanInfrastructureEndpointsVmKeyAdditionsCreateKeyAdditionEndpointParams{IdempotencyKey: command.NewIdempotencyKey()}
			body := api.CreateKeyAdditionBody{SshKeyIds: &keyIds}
			response, err := client.TitanInfrastructureEndpointsVmKeyAdditionsCreateKeyAdditionEndpointWithResponse(ctx, id, params, body)
			if err != nil {
				return err
			}
			if response.JSON202 == nil {
				return output.Explain(api.ProblemOf(response.HTTPResponse, response.Body))
			}
			keyAdditionId := *response.JSON202.KeyAdditionId
			if noWait {
				deps.Printer.Message("Adding %d SSH key(s) to VM %s.", len(keyIds), name)
				return deps.Printer.Object([]output.Field{{Name: "ID", Value: keyAdditionId}}, response.JSON202)
			}
			deps.Printer.Message("Adding %d SSH key(s) to VM %s, waiting until it is Added or Failed...", len(keyIds), name)
			addition, err := waitForKeyAddition(ctx, deps, client, id, name, keyAdditionId, timeout)
			if err != nil {
				return err
			}
			return printKeyAddition(deps, name, addition)
		},
	}
	cmd.Flags().StringArrayVar(&sshKeys, "ssh-key", nil, "SSH key name or id to add; repeat for several keys (1 to 10)")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "do not wait for the addition to end; print its id instead")
	cmd.Flags().DurationVar(&timeout, "timeout", defaultTimeout, "how long to wait for the addition to end")
	return cmd
}

// dedupeStrings keeps the first occurrence of each value, in order: a key named or id'd twice on the command line
// counts once toward the 1-10 limit, before any key is even resolved.
func dedupeStrings(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// resolveSshKeys turns each name or id into the key's id, as vm create does, then dedupes again: a key given once
// by name and once by its own id must still count and be sent once.
func resolveSshKeys(ctx context.Context, client *api.ClientWithResponses, namesOrIds []string) ([]string, error) {
	seen := map[string]bool{}
	ids := []string{}
	for _, nameOrId := range namesOrIds {
		key, err := sshkey.Resolve(ctx, client, nameOrId)
		if err != nil {
			return nil, err
		}
		if seen[*key.Id] {
			continue
		}
		seen[*key.Id] = true
		ids = append(ids, *key.Id)
	}
	return ids, nil
}

// waitForKeyAddition polls the addition until it is Added or Failed: unlike VM status, there is no transitional word
// to look past, so any status the CLI does not recognize as an end state is still "waiting" (titan-ops
// features/vm/design.md#vm-key-addition).
func waitForKeyAddition(ctx context.Context, deps *command.Deps, client *api.ClientWithResponses, vmId string, vmName string, keyAdditionId string, timeout time.Duration) (*api.VmKeyAdditionQueryResult, error) {
	var last *api.VmKeyAdditionQueryResult
	err := wait.Until(ctx, deps.Clock, timeout, pollInterval, func(ctx context.Context) (bool, error) {
		response, err := client.TitanInfrastructureEndpointsVmKeyAdditionsGetKeyAdditionEndpointWithResponse(ctx, vmId, keyAdditionId)
		if err != nil {
			return false, err
		}
		if response.JSON200 == nil {
			return false, output.Explain(api.ProblemOf(response.HTTPResponse, response.Body))
		}
		last = response.JSON200
		ended := last.Status != nil && (*last.Status == api.VmKeyAdditionStatusAdded || *last.Status == api.VmKeyAdditionStatusFailed)
		return ended, nil
	})
	if errors.Is(err, wait.ErrTimeout) {
		return nil, fmt.Errorf("the key addition to VM %s is still %s after %s", vmName, output.Word(last.Status), timeout)
	}
	if err != nil {
		return nil, err
	}
	if *last.Status == api.VmKeyAdditionStatusFailed {
		reason := "the request failed"
		if last.RequestFailureReason != nil {
			reason = *last.RequestFailureReason
		}
		return nil, fmt.Errorf("adding the SSH key(s) to VM %s failed: %s", vmName, reason)
	}
	return last, nil
}

func printKeyAddition(deps *command.Deps, vmName string, addition *api.VmKeyAdditionQueryResult) error {
	keyIds := []string{}
	if addition.SshKeyIds != nil {
		keyIds = *addition.SshKeyIds
	}
	restartNeeded := addition.RestartNeeded != nil && *addition.RestartNeeded
	fields := []output.Field{
		{Name: "ID", Value: output.Text(addition.Id)},
		{Name: "Status", Value: output.Word(addition.Status)},
		{Name: "SSH keys", Value: strings.Join(keyIds, ", ")},
		{Name: "Restart needed", Value: strconv.FormatBool(restartNeeded)},
	}
	if err := deps.Printer.Object(fields, addition); err != nil {
		return err
	}
	if restartNeeded {
		deps.Printer.Message("Stop VM %s and start it again for the new key to take effect: `openvicloud vm stop %s --wait` then `openvicloud vm start %s`.",
			vmName, vmName, vmName)
	}
	return nil
}
