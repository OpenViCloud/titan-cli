// Package sshkey is `openvicloud ssh-key`: the public keys of the tenant, picked when a VM is created
// (titan-ops features/vm/design.md#ssh-key).
package sshkey

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/OpenViCloud/titan-cli/api"
	"github.com/OpenViCloud/titan-cli/command"
	"github.com/OpenViCloud/titan-cli/output"
)

func New(deps *command.Deps) *cobra.Command {
	sshKey := &cobra.Command{
		Use:   "ssh-key",
		Short: "Your public SSH keys, for logging in to VMs",
	}
	sshKey.AddCommand(newAdd(deps), newList(deps), newDelete(deps))
	return sshKey
}

func newAdd(deps *command.Deps) *cobra.Command {
	var file string
	add := &cobra.Command{
		Use:   "add <name> --public-key-file <file>",
		Short: "Add a public key, for example ~/.ssh/id_ed25519.pub",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			content, err := os.ReadFile(file)
			if err != nil {
				return fmt.Errorf("cannot read the public key file %s: %w", file, err)
			}
			client, err := deps.Client(cmd.Context())
			if err != nil {
				return err
			}
			name := args[0]
			publicKey := strings.TrimSpace(string(content))
			params := &api.TitanInfrastructureEndpointsSshKeyAddSshKeyEndpointParams{IdempotencyKey: command.NewIdempotencyKey()}
			body := api.AddSshKeyCommand{Name: &name, PublicKey: &publicKey}
			response, err := client.TitanInfrastructureEndpointsSshKeyAddSshKeyEndpointWithResponse(cmd.Context(), params, body)
			if err != nil {
				return err
			}
			if response.JSON201 == nil {
				return output.Explain(api.ProblemOf(response.HTTPResponse, response.Body))
			}
			return deps.Printer.Object([]output.Field{{Name: "ID", Value: output.Text(response.JSON201.Id)}}, response.JSON201)
		},
	}
	add.Flags().StringVar(&file, "public-key-file", "", "the public key, one line as in ~/.ssh/id_ed25519.pub")
	add.MarkFlagRequired("public-key-file")
	return add
}

func newList(deps *command.Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List your SSH keys",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := deps.Client(cmd.Context())
			if err != nil {
				return err
			}
			keys, err := List(cmd.Context(), client)
			if err != nil {
				return err
			}
			rows := [][]string{}
			for _, key := range keys {
				rows = append(rows, []string{output.Text(key.Name), output.Text(key.Id), output.Text(key.Type), output.Text(key.Fingerprint)})
			}
			return deps.Printer.Table([]string{"NAME", "ID", "TYPE", "FINGERPRINT"}, rows, keys)
		},
	}
}

func newDelete(deps *command.Deps) *cobra.Command {
	var yes bool
	remove := &cobra.Command{
		Use:   "delete <key name or id>",
		Short: "Delete an SSH key (VMs that already have it keep it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := deps.Client(cmd.Context())
			if err != nil {
				return err
			}
			key, err := Resolve(cmd.Context(), client, args[0])
			if err != nil {
				return err
			}
			if !yes && !deps.Confirm(fmt.Sprintf("Delete SSH key %s (%s)?", output.Text(key.Name), *key.Id)) {
				return fmt.Errorf("the SSH key was not deleted")
			}
			response, err := client.TitanInfrastructureEndpointsSshKeyDeleteSshKeyEndpointWithResponse(cmd.Context(), *key.Id)
			if err != nil {
				return err
			}
			if response.StatusCode() != http.StatusNoContent {
				return output.Explain(api.ProblemOf(response.HTTPResponse, response.Body))
			}
			deps.Printer.Message("SSH key %s deleted.", output.Text(key.Name))
			return nil
		},
	}
	remove.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	return remove
}

func List(ctx context.Context, client *api.ClientWithResponses) ([]api.SshKeyQueryResult, error) {
	response, err := client.TitanInfrastructureEndpointsSshKeyListSshKeysEndpointWithResponse(ctx)
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, output.Explain(api.ProblemOf(response.HTTPResponse, response.Body))
	}
	return *response.JSON200, nil
}

// Resolve finds the tenant's key whose id or name is nameOrId.
func Resolve(ctx context.Context, client *api.ClientWithResponses, nameOrId string) (api.SshKeyQueryResult, error) {
	keys, err := List(ctx, client)
	if err != nil {
		return api.SshKeyQueryResult{}, err
	}
	for _, key := range keys {
		if key.Id == nil {
			continue
		}
		matches := *key.Id == nameOrId || (key.Name != nil && *key.Name == nameOrId)
		if matches {
			return key, nil
		}
	}
	return api.SshKeyQueryResult{}, fmt.Errorf("no SSH key named %q in your tenant: see `openvicloud ssh-key list`", nameOrId)
}
