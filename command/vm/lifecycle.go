package vm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/OpenViCloud/titan-cli/api"
	"github.com/OpenViCloud/titan-cli/command"
	"github.com/OpenViCloud/titan-cli/command/catalog"
	"github.com/OpenViCloud/titan-cli/command/sshkey"
	"github.com/OpenViCloud/titan-cli/output"
	"github.com/OpenViCloud/titan-cli/wait"
)

const (
	pollInterval   = 5 * time.Second
	defaultTimeout = 10 * time.Minute
	// A create whose answer was lost (no answer, 502/503/504) is sent again with the same Idempotency-Key, so the
	// server makes one VM however many times it arrives (https://github.com/OpenViCloud/titan-ops/blob/main/docs/architecture/http-contract.md#idempotency-key).
	createAttempts = 3
	retryPause     = 2 * time.Second
)

// goal is what a request ends in: once the VM has no request waiting (transition is null), reached says whether it
// got there (https://github.com/OpenViCloud/titan-ops/blob/main/docs/features/cli/design.md#vm-wait).
type goal struct {
	want    string
	reached func(vm *api.VmQueryResult) bool
}

var (
	running = goal{want: "Running or Ready", reached: func(vm *api.VmQueryResult) bool {
		return vm.CurrentState != nil && (*vm.CurrentState == api.VmStateRunning || *vm.CurrentState == api.VmStateReady)
	}}
	stopped = goal{want: "Stopped", reached: func(vm *api.VmQueryResult) bool {
		return vm.CurrentState != nil && *vm.CurrentState == api.VmStateStopped
	}}
	deleted = goal{want: "deleted", reached: func(vm *api.VmQueryResult) bool {
		return vm.CurrentExistence != nil && *vm.CurrentExistence == api.CurrentExistenceDeleted
	}}
)

type waitFlags struct {
	wait    bool
	timeout time.Duration
}

func (w *waitFlags) add(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&w.wait, "wait", false, "wait until the request is done")
	cmd.Flags().DurationVar(&w.timeout, "timeout", defaultTimeout, "how long --wait waits")
}

func newList(deps *command.Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List your VMs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := deps.Client(cmd.Context())
			if err != nil {
				return err
			}
			vms, err := list(cmd.Context(), client)
			if err != nil {
				return err
			}
			rows := [][]string{}
			for _, vm := range vms {
				rows = append(rows, []string{output.Text(vm.Name), output.Text(vm.Id), output.Text(vm.ClusterId),
					output.Word(vm.CurrentState), output.Word(vm.Transition), output.Text(vm.IpAddress)})
			}
			return deps.Printer.Table([]string{"NAME", "ID", "CLUSTER", "STATE", "WAITING FOR", "IPV4"}, rows, vms)
		},
	}
}

func newGet(deps *command.Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "get <vm name or id>",
		Short: "Show one VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := deps.Client(cmd.Context())
			if err != nil {
				return err
			}
			id, _, err := resolve(cmd.Context(), client, args[0])
			if err != nil {
				return err
			}
			vm, err := get(cmd.Context(), client, id)
			if err != nil {
				return err
			}
			return printVm(deps, vm)
		},
	}
}

func newCreate(deps *command.Deps) *cobra.Command {
	var cluster, network, template, size string
	var sshKeys []string
	var diskSizeGb int32
	var flags waitFlags
	create := &cobra.Command{
		Use:   "create <name> --cluster <cluster> --network <network> --template <template> --size <size>",
		Short: "Create a VM",
		Long: "Create a VM. Network, template, size and SSH keys are given by name or id: see `openvicloud template list`,\n" +
			"`openvicloud size list`, `openvicloud ssh-key list`. Without an SSH key you cannot log in to the VM.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, err := deps.Client(ctx)
			if err != nil {
				return err
			}
			body := api.CreateVmCommand{Name: args[0], ClusterId: cluster}
			body.NetworkId, err = resolveNetwork(ctx, client, network)
			if err != nil {
				return err
			}
			body.TemplateId, err = resolveTemplate(ctx, client, cluster, template)
			if err != nil {
				return err
			}
			body.VmOfferingId, err = resolveSize(ctx, client, size)
			if err != nil {
				return err
			}
			keyIds := []string{}
			for _, nameOrId := range sshKeys {
				key, err := sshkey.Resolve(ctx, client, nameOrId)
				if err != nil {
					return err
				}
				keyIds = append(keyIds, *key.Id)
			}
			if len(keyIds) > 0 {
				body.SshKeyIds = &keyIds
			}
			if diskSizeGb > 0 {
				body.DiskSizeGb = &diskSizeGb
			}
			created, err := createVm(ctx, deps, client, body)
			if err != nil {
				return err
			}
			id := *created.VmId
			if !flags.wait {
				deps.Printer.Message("Creating VM %s.", body.Name)
				return deps.Printer.Object([]output.Field{{Name: "ID", Value: id}}, created)
			}
			deps.Printer.Message("Creating VM %s, waiting until it is %s...", body.Name, running.want)
			vm, err := waitFor(ctx, deps, client, id, body.Name, running, flags.timeout)
			if err != nil {
				return err
			}
			return printVm(deps, vm)
		},
	}
	create.Flags().StringVar(&cluster, "cluster", "", "cluster id (see `openvicloud cluster list`)")
	create.Flags().StringVar(&network, "network", "", "network name or id, in that cluster")
	create.Flags().StringVar(&template, "template", "", "template name or id (see `openvicloud template list`)")
	create.Flags().StringVar(&size, "size", "", "size name or id (see `openvicloud size list`)")
	create.Flags().StringArrayVar(&sshKeys, "ssh-key", nil, "SSH key name or id to install; repeat for several keys")
	create.Flags().Int32Var(&diskSizeGb, "disk-size", 0, "disk size in GB (default: the template's)")
	for _, name := range []string{"cluster", "network", "template", "size"} {
		create.MarkFlagRequired(name)
	}
	flags.add(create)
	return create
}

// createVm sends the create, again with the same key while its answer is lost.
func createVm(ctx context.Context, deps *command.Deps, client *api.ClientWithResponses, body api.CreateVmCommand) (*api.CreateVmCommandResult, error) {
	params := &api.TitanInfrastructureEndpointsVmCreateVmEndpointParams{IdempotencyKey: command.NewIdempotencyKey()}
	var lastErr error
	for attempt := 1; attempt <= createAttempts; attempt++ {
		if attempt > 1 {
			deps.Clock.Sleep(retryPause)
		}
		response, err := client.TitanInfrastructureEndpointsVmCreateVmEndpointWithResponse(ctx, params, body)
		if err != nil {
			lastErr = err
			continue
		}
		if response.JSON202 != nil {
			return response.JSON202, nil
		}
		lastErr = output.Explain(api.ProblemOf(response.HTTPResponse, response.Body))
		answerLost := response.StatusCode() == http.StatusBadGateway || response.StatusCode() == http.StatusServiceUnavailable ||
			response.StatusCode() == http.StatusGatewayTimeout
		if !answerLost {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

func newRename(deps *command.Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "rename <vm name or id> <new name>",
		Short: "Rename a VM",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := deps.Client(cmd.Context())
			if err != nil {
				return err
			}
			id, name, err := resolve(cmd.Context(), client, args[0])
			if err != nil {
				return err
			}
			response, err := client.TitanInfrastructureEndpointsVmUpdateVmEndpointWithResponse(cmd.Context(), id, api.RenameVmBody{Name: args[1]})
			if err != nil {
				return err
			}
			if response.StatusCode() != http.StatusNoContent {
				return output.Explain(api.ProblemOf(response.HTTPResponse, response.Body))
			}
			deps.Printer.Message("VM %s renamed to %s.", name, args[1])
			return nil
		},
	}
}

// request is stop, start or delete: one call that answers 202, then optionally the wait for its goal.
type request struct {
	verb  string
	short string
	goal  goal
	send  func(ctx context.Context, client *api.ClientWithResponses, id string) (*http.Response, []byte, error)
}

func newRequest(deps *command.Deps, r request) *cobra.Command {
	var flags waitFlags
	var yes bool
	cmd := &cobra.Command{
		Use:   r.verb + " <vm name or id>",
		Short: r.short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, err := deps.Client(ctx)
			if err != nil {
				return err
			}
			id, name, err := resolve(ctx, client, args[0])
			if err != nil {
				return err
			}
			isDelete := r.verb == "delete"
			if isDelete && !yes && !deps.Confirm(fmt.Sprintf("Delete VM %s (%s)? Its disk is deleted with it.", name, id)) {
				return fmt.Errorf("the VM was not deleted")
			}
			httpResponse, responseBody, err := r.send(ctx, client, id)
			if err != nil {
				return err
			}
			if httpResponse.StatusCode != http.StatusAccepted {
				return output.Explain(api.ProblemOf(httpResponse, responseBody))
			}
			if !flags.wait {
				deps.Printer.Message("Request to %s VM %s sent.", r.verb, name)
				return nil
			}
			deps.Printer.Message("Request to %s VM %s sent, waiting until it is %s...", r.verb, name, r.goal.want)
			vm, err := waitFor(ctx, deps, client, id, name, r.goal, flags.timeout)
			if err != nil {
				return err
			}
			if isDelete {
				deps.Printer.Message("VM %s deleted.", name)
				return nil
			}
			return printVm(deps, vm)
		},
	}
	flags.add(cmd)
	if r.verb == "delete" {
		cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	}
	return cmd
}

func newStart(deps *command.Deps) *cobra.Command {
	return newRequest(deps, request{verb: "start", short: "Start a stopped (or resume a frozen) VM", goal: running,
		send: func(ctx context.Context, client *api.ClientWithResponses, id string) (*http.Response, []byte, error) {
			response, err := client.TitanInfrastructureEndpointsVmStartVmEndpointWithResponse(ctx, id)
			if err != nil {
				return nil, nil, err
			}
			return response.HTTPResponse, response.Body, nil
		}})
}

func newStop(deps *command.Deps) *cobra.Command {
	return newRequest(deps, request{verb: "stop", short: "Stop a VM (its disk stays, compute is no longer billed)", goal: stopped,
		send: func(ctx context.Context, client *api.ClientWithResponses, id string) (*http.Response, []byte, error) {
			response, err := client.TitanInfrastructureEndpointsVmStopVmEndpointWithResponse(ctx, id)
			if err != nil {
				return nil, nil, err
			}
			return response.HTTPResponse, response.Body, nil
		}})
}

func newDelete(deps *command.Deps) *cobra.Command {
	return newRequest(deps, request{verb: "delete", short: "Delete a VM and its disk", goal: deleted,
		send: func(ctx context.Context, client *api.ClientWithResponses, id string) (*http.Response, []byte, error) {
			response, err := client.TitanInfrastructureEndpointsVmDeleteVmEndpointWithResponse(ctx, id)
			if err != nil {
				return nil, nil, err
			}
			return response.HTTPResponse, response.Body, nil
		}})
}

// waitFor reads the VM until it has no request waiting, then says whether the request reached its goal.
func waitFor(ctx context.Context, deps *command.Deps, client *api.ClientWithResponses, id string, name string, g goal, timeout time.Duration) (*api.VmQueryResult, error) {
	var last *api.VmQueryResult
	err := wait.Until(ctx, deps.Clock, timeout, pollInterval, func(ctx context.Context) (bool, error) {
		vm, err := get(ctx, client, id)
		if err != nil {
			return false, err
		}
		last = vm
		return vm.Transition == nil, nil
	})
	if errors.Is(err, wait.ErrTimeout) {
		return nil, fmt.Errorf("VM %s is still %s after %s: the request goes on, check it with `openvicloud vm get %s`",
			name, output.Word(last.Transition), timeout, name)
	}
	if err != nil {
		return nil, err
	}
	if g.reached(last) {
		return last, nil
	}
	if last.RequestFailureReason != nil {
		return nil, fmt.Errorf("the request for VM %s failed: %s", name, *last.RequestFailureReason)
	}
	return nil, fmt.Errorf("VM %s is %s, not %s: the request did not complete", name, output.Word(last.CurrentState), g.want)
}

func list(ctx context.Context, client *api.ClientWithResponses) ([]api.VmQueryResult, error) {
	response, err := client.TitanInfrastructureEndpointsVmListVmsEndpointWithResponse(ctx)
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, output.Explain(api.ProblemOf(response.HTTPResponse, response.Body))
	}
	return *response.JSON200, nil
}

func get(ctx context.Context, client *api.ClientWithResponses, id string) (*api.VmQueryResult, error) {
	response, err := client.TitanInfrastructureEndpointsVmGetVmEndpointWithResponse(ctx, id)
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, output.Explain(api.ProblemOf(response.HTTPResponse, response.Body))
	}
	return response.JSON200, nil
}

func printVm(deps *command.Deps, vm *api.VmQueryResult) error {
	fields := []output.Field{
		{Name: "ID", Value: output.Text(vm.Id)},
		{Name: "Name", Value: output.Text(vm.Name)},
		{Name: "Cluster", Value: output.Text(vm.ClusterId)},
		{Name: "State", Value: output.Word(vm.CurrentState)},
		{Name: "Requested state", Value: output.Word(vm.DesiredState)},
		{Name: "Waiting for", Value: output.Word(vm.Transition)},
		{Name: "IPv4", Value: output.Text(vm.IpAddress)},
		{Name: "Public IPv6", Value: output.Text(vm.PublicIpv6Address)},
		{Name: "Network", Value: output.Text(vm.NetworkId)},
		{Name: "Size", Value: output.Text(vm.VmOfferingId)},
		{Name: "Template", Value: output.Text(vm.TemplateId)},
		{Name: "Disk GB", Value: output.Int(vm.DiskSizeGb)},
		{Name: "Created", Value: output.Time(vm.CreatedAt)},
	}
	if vm.RequestFailureReason != nil {
		fields = append(fields, output.Field{Name: "Request failed", Value: *vm.RequestFailureReason})
	}
	return deps.Printer.Object(fields, vm)
}

func resolveNetwork(ctx context.Context, client *api.ClientWithResponses, nameOrId string) (string, error) {
	response, err := client.TitanInfrastructureEndpointsNetworkListNetworksEndpointWithResponse(ctx)
	if err != nil {
		return "", err
	}
	if response.JSON200 == nil {
		return "", output.Explain(api.ProblemOf(response.HTTPResponse, response.Body))
	}
	for _, network := range *response.JSON200 {
		if network.Id == nil {
			continue
		}
		if *network.Id == nameOrId || (network.Name != nil && *network.Name == nameOrId) {
			return *network.Id, nil
		}
	}
	return "", fmt.Errorf("no network named %q in your tenant: see your networks on the portal", nameOrId)
}

func resolveTemplate(ctx context.Context, client *api.ClientWithResponses, cluster string, nameOrId string) (string, error) {
	templates, err := catalog.Templates(ctx, client, cluster)
	if err != nil {
		return "", err
	}
	for _, template := range templates {
		if template.Id == nil {
			continue
		}
		if *template.Id == nameOrId || (template.Name != nil && *template.Name == nameOrId) {
			return *template.Id, nil
		}
	}
	return "", fmt.Errorf("no template named %q in cluster %s: see `openvicloud template list --cluster %s`", nameOrId, cluster, cluster)
}

func resolveSize(ctx context.Context, client *api.ClientWithResponses, nameOrId string) (string, error) {
	sizes, err := catalog.Sizes(ctx, client)
	if err != nil {
		return "", err
	}
	for _, size := range sizes {
		if size.Id == nil {
			continue
		}
		if *size.Id == nameOrId || (size.Name != nil && *size.Name == nameOrId) {
			return *size.Id, nil
		}
	}
	return "", fmt.Errorf("no size named %q: see `openvicloud size list`", nameOrId)
}
