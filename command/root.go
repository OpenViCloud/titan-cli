// Package command holds the root command and what every subcommand shares (Deps). Subcommands live in
// command/<resource> and import this package; main.go is the only place that attaches them to the root.
package command

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/OpenViCloud/titan-cli/api"
	"github.com/OpenViCloud/titan-cli/output"
	"github.com/OpenViCloud/titan-cli/session"
	"github.com/OpenViCloud/titan-cli/wait"
)

// The CLI targets OpenViCloud's production hosts; each default is overridden by its environment variable.
const (
	defaultApi    = "https://api.openvicloud.com"
	defaultAuth   = "https://auth.openvicloud.com/auth/realms/titan"
	defaultPortal = "https://portal.openvicloud.com"
)

type Deps struct {
	Endpoints   session.Endpoints
	Sessions    *session.Manager
	Printer     *output.Printer
	HTTP        *http.Client
	Version     string
	OpenBrowser func(url string) error
	// For `vm ssh` and its tunnel: the tunnel reads ssh's bytes from Stdin; `vm ssh` starts the system ssh and
	// names this binary as its ProxyCommand.
	Stdin      io.Reader
	Executable func() (string, error)
	RunSSH     func(ctx context.Context, args []string) error
	// For --wait and for retrying a create.
	Clock wait.Clock
	// From OPENVICLOUD_TOKEN: when set, every request carries it instead of the login session, for CI and scripts
	// (https://github.com/OpenViCloud/titan-ops/blob/main/docs/features/cli/design.md#api-token).
	ApiToken string
}

// Confirm asks the person at the terminal a yes/no question; only "y" or "yes" is yes. No answer (end of input) is
// no.
func (d *Deps) Confirm(question string) bool {
	d.Printer.Message("%s [y/N]", question)
	line, _ := bufio.NewReader(d.Stdin).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// NewIdempotencyKey is the key of one create: sent with every attempt of that create, never with another
// (https://github.com/OpenViCloud/titan-ops/blob/main/docs/architecture/http-contract.md#idempotency-key).
func NewIdempotencyKey() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

// ExitCode is the error of a command whose exit code is another program's (ssh): main exits with it and prints
// nothing, the program already said why.
type ExitCode struct {
	Code int
}

func (e *ExitCode) Error() string {
	return fmt.Sprintf("exit status %d", e.Code)
}

// ExitCodeOf is the exit code to pass on for an ssh that ended with code: -1 (killed by a signal, no exit status)
// becomes 255, the code ssh uses for its own errors.
func ExitCodeOf(code int) int {
	if code < 0 {
		return 255
	}
	return code
}

// RunSSH runs the system ssh with the terminal of this process. Not tied to ctx: Ctrl-C reaches ssh from the terminal,
// and killing it instead would leave the terminal without echo at a passphrase prompt.
func RunSSH(ctx context.Context, args []string) error {
	ssh := exec.Command("ssh", args...)
	ssh.Stdin = os.Stdin
	ssh.Stdout = os.Stdout
	ssh.Stderr = os.Stderr
	err := ssh.Run()
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		return &ExitCode{Code: ExitCodeOf(exited.ExitCode())}
	}
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("ssh is not installed or not on PATH")
	}
	return err
}

func EndpointsFromEnv(getenv func(string) string) session.Endpoints {
	return session.Endpoints{
		Api:    valueOr(getenv("OPENVICLOUD_API_URL"), defaultApi),
		Auth:   valueOr(getenv("OPENVICLOUD_AUTH_URL"), defaultAuth),
		Portal: valueOr(getenv("OPENVICLOUD_PORTAL_URL"), defaultPortal),
	}
}

// ApiTokenFromEnv is OPENVICLOUD_TOKEN without the spaces or newline a copy and paste brings along.
func ApiTokenFromEnv(getenv func(string) string) string {
	return strings.TrimSpace(getenv("OPENVICLOUD_TOKEN"))
}

func valueOr(value string, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func NewRoot(deps *Deps) *cobra.Command {
	root := &cobra.Command{
		Use:   "openvicloud",
		Short: "Manage your OpenViCloud resources from the terminal",
		Long: "Manage your OpenViCloud resources from the terminal.\n\n" +
			"Start with `openvicloud auth login`. Paying invoices and topping up the wallet are done on the portal.",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return output.ValidFormat(deps.Printer.Format)
		},
	}
	root.PersistentFlags().StringVarP(&deps.Printer.Format, "output", "o", output.FormatTable, "output format: table or json")
	return root
}

// AccessToken is the API token when one is set, else the logged-in customer's token for this endpoint, or the error
// that says how to log in.
func (d *Deps) AccessToken(ctx context.Context) (string, error) {
	if d.ApiToken != "" {
		return d.ApiToken, nil
	}
	token, err := d.Sessions.AccessToken(ctx)
	if err != nil {
		return "", d.explainSession(err)
	}
	return token, nil
}

// Client is an API client for a customer who is logged in to this endpoint and has accepted the current
// terms. Every command that needs a login starts here.
func (d *Deps) Client(ctx context.Context) (*api.ClientWithResponses, error) {
	token, err := d.AccessToken(ctx)
	if err != nil {
		return nil, err
	}
	client, err := api.New(d.Endpoints.Api, token, d.Version, d.HTTP)
	if err != nil {
		return nil, err
	}
	mustAccept := d.mustAcceptTerms(ctx, client)
	if mustAccept {
		return nil, fmt.Errorf("you have not accepted the current terms of service: accept them at %s, then run the command again", d.Endpoints.Portal)
	}
	return client, nil
}

// mustAcceptTerms mirrors the portal's consent gate (https://github.com/OpenViCloud/titan-ops/blob/main/docs/features/legal/design.md#consent-gate-per-user):
// when the status cannot be read the command runs, as the portal shows the app.
func (d *Deps) mustAcceptTerms(ctx context.Context, client *api.ClientWithResponses) bool {
	response, err := client.TitanInfrastructureEndpointsLegalGetConsentStatusEndpointWithResponse(ctx)
	if err != nil {
		return false
	}
	if response.JSON200 == nil {
		return false
	}
	if response.JSON200.MustAcceptTerms == nil {
		return false
	}
	return *response.JSON200.MustAcceptTerms
}

// TermsReminder tells a customer who just logged in that the terms still need accepting. It never fails the
// login.
func (d *Deps) TermsReminder(ctx context.Context) {
	token, err := d.Sessions.AccessToken(ctx)
	if err != nil {
		return
	}
	client, err := api.New(d.Endpoints.Api, token, d.Version, d.HTTP)
	if err != nil {
		return
	}
	mustAccept := d.mustAcceptTerms(ctx, client)
	if mustAccept {
		d.Printer.Message("Before you use the CLI, accept the current terms of service at %s", d.Endpoints.Portal)
	}
}

func (d *Deps) explainSession(err error) error {
	if errors.Is(err, session.ErrNotLoggedIn) {
		return fmt.Errorf("you are not logged in: run `openvicloud auth login`")
	}
	if errors.Is(err, session.ErrSessionExpired) {
		return fmt.Errorf("your session has expired: run `openvicloud auth login`")
	}
	if errors.Is(err, session.ErrOtherEndpoint) {
		return fmt.Errorf("you are logged in to another endpoint, not %s: run `openvicloud auth login`", d.Endpoints.Api)
	}
	return err
}

// OpenBrowser opens url in the default browser of the machine.
func OpenBrowser(url string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", url)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		command = exec.Command("xdg-open", url)
	}
	return command.Start()
}
