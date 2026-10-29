// Package vm is `openvicloud vm`: the VM lifecycle (lifecycle.go), and `vm ssh` with the tunnel it runs as ssh's
// ProxyCommand.
//
// A VM has no public address: ssh reaches it through a websocket to the control plane, which runs nc to port 22
// inside the guest (titan-ops features/vm/design.md#ssh-session). `vm ssh` starts the system ssh with that tunnel; the
// customer's key and known_hosts stay ssh's business.
package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"

	"github.com/OpenViCloud/titan-cli/api"
	"github.com/OpenViCloud/titan-cli/command"
	"github.com/OpenViCloud/titan-cli/output"
)

// The control plane sends up to 64 KiB per message; the library's default limit is 32 KiB.
const readLimit = 1 << 20

func New(deps *command.Deps) *cobra.Command {
	vm := &cobra.Command{
		Use:   "vm",
		Short: "Your virtual machines",
	}
	vm.AddCommand(newList(deps), newGet(deps), newCreate(deps), newRename(deps), newStart(deps), newStop(deps),
		newDelete(deps), newSsh(deps), newTunnel(deps))
	return vm
}

func newSsh(deps *command.Deps) *cobra.Command {
	var user string
	ssh := &cobra.Command{
		Use:   "ssh <vm name or id> [-- <ssh options>]",
		Short: "Open an SSH session to a running VM",
		Long: "Open an SSH session to a running VM with your system ssh, through OpenViCloud: the VM needs no public address.\n\n" +
			"Your VM must have one of your SSH keys (pick it when you create the VM). Anything after -- goes to ssh after the\n" +
			"destination: options, then a command to run, for example:\n" +
			"  openvicloud vm ssh web-1 -- -i ~/.ssh/id_ed25519\n" +
			"  openvicloud vm ssh web-1 -- uptime",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := deps.Client(cmd.Context())
			if err != nil {
				return err
			}
			id, name, err := resolve(cmd.Context(), client, args[0])
			if err != nil {
				return err
			}
			self, err := deps.Executable()
			if err != nil {
				return err
			}
			return deps.RunSSH(cmd.Context(), SshArgs(self, id, name, user, args[1:]))
		},
	}
	ssh.Flags().StringVar(&user, "user", "ubuntu", "user to log in as")
	return ssh
}

// SshArgs are the arguments of the system ssh: the tunnel as ProxyCommand, and a host key alias per VM so
// known_hosts tells VMs apart although every one is reached the same way. The destination comes before extra: ssh
// still reads options after it, and the first word that is not an option starts the remote command.
func SshArgs(self string, id string, name string, user string, extra []string) []string {
	args := []string{
		"-o", "ProxyCommand=" + quoteFor(runtime.GOOS, self) + " vm tunnel " + id,
		"-o", "HostKeyAlias=openvicloud-" + id,
		user + "@" + name,
	}
	return append(args, extra...)
}

// ssh expands % tokens in ProxyCommand, then runs it through /bin/sh on Unix and as a plain process on Windows, where
// single quotes are not quotes. Always quoted: a path may hold any character the shell reads (&, ;, *, ...), and a
// Windows path cannot hold a double quote.
func quoteFor(goos string, path string) string {
	escaped := strings.ReplaceAll(path, "%", "%%")
	if goos == "windows" {
		return `"` + escaped + `"`
	}
	return "'" + strings.ReplaceAll(escaped, "'", `'\''`) + "'"
}

func resolve(ctx context.Context, client *api.ClientWithResponses, nameOrId string) (string, string, error) {
	response, err := client.TitanInfrastructureEndpointsVmListVmsEndpointWithResponse(ctx)
	if err != nil {
		return "", "", err
	}
	if response.JSON200 == nil {
		return "", "", output.Explain(api.ProblemOf(response.HTTPResponse, response.Body))
	}
	for _, vm := range *response.JSON200 {
		if vm.Id == nil || vm.Name == nil {
			continue
		}
		matches := *vm.Id == nameOrId || *vm.Name == nameOrId
		if matches {
			return *vm.Id, *vm.Name, nil
		}
	}
	return "", "", fmt.Errorf("no VM named %q in your tenant: see your VMs on the portal", nameOrId)
}

func newTunnel(deps *command.Deps) *cobra.Command {
	return &cobra.Command{
		Use:    "tunnel <vm id>",
		Short:  "Carry ssh's bytes to a VM (ssh runs this as its ProxyCommand)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// ssh ends its ProxyCommand with a signal: stop then, and close the session normally.
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGHUP, syscall.SIGTERM)
			defer stop()
			return Tunnel(ctx, deps, args[0], deps.Stdin, deps.Printer.Out)
		},
	}
}

// Tunnel copies in to the VM's port 22 and the VM's answer to out, until either side ends or ctx is done; then the
// websocket is closed normally, which ends the command in the guest at once.
func Tunnel(ctx context.Context, deps *command.Deps, vmId string, in io.Reader, out io.Writer) error {
	token, err := deps.AccessToken(ctx)
	if err != nil {
		return err
	}
	url := websocketBase(deps.Endpoints.Api) + "/v1/tenant/vms/" + vmId + "/ssh"
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	header.Set("User-Agent", "openvicloud-cli/"+deps.Version)
	conn, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		refused := response != nil && response.StatusCode != http.StatusSwitchingProtocols
		if refused {
			body, _ := io.ReadAll(response.Body)
			return output.Explain(api.ProblemOf(response, body))
		}
		return fmt.Errorf("could not reach %s: %w", deps.Endpoints.Api, err)
	}
	conn.SetReadLimit(readLimit)
	// Not ctx: a done ctx would drop the connection instead of closing it.
	stream := websocket.NetConn(context.Background(), conn, websocket.MessageBinary)
	defer stream.Close()

	downDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(out, stream)
		downDone <- err
	}()
	upDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(stream, in)
		upDone <- err
	}()

	select {
	case err := <-downDone:
		return endOfSession(err)
	case <-upDone:
		// ssh closed our stdin: the session is over on its side.
	case <-ctx.Done():
	}
	conn.Close(websocket.StatusNormalClosure, "")
	select {
	case <-downDone:
	case <-time.After(2 * time.Second):
	}
	return nil
}

// The control plane closing the websocket is how a session ends.
func endOfSession(err error) error {
	if err == nil {
		return nil
	}
	status := websocket.CloseStatus(err)
	if status == websocket.StatusNormalClosure {
		return nil
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func websocketBase(api string) string {
	if strings.HasPrefix(api, "https://") {
		return "wss://" + strings.TrimPrefix(api, "https://")
	}
	if strings.HasPrefix(api, "http://") {
		return "ws://" + strings.TrimPrefix(api, "http://")
	}
	return api
}
