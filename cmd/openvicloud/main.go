// openvicloud is the command-line client of OpenViCloud. main.go is the only place that knows every package:
// it builds the dependencies and attaches each command to the root.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/OpenViCloud/titan-cli/command"
	"github.com/OpenViCloud/titan-cli/command/auth"
	"github.com/OpenViCloud/titan-cli/command/catalog"
	"github.com/OpenViCloud/titan-cli/command/sshkey"
	"github.com/OpenViCloud/titan-cli/command/tenant"
	"github.com/OpenViCloud/titan-cli/command/vm"
	"github.com/OpenViCloud/titan-cli/output"
	"github.com/OpenViCloud/titan-cli/session"
	"github.com/OpenViCloud/titan-cli/wait"
)

// version is set at build time: -ldflags "-X main.version=<semver>".
var version = "dev"

// buildVersion is the linked version of a release build. A `go install …@vX.Y.Z` has no -ldflags, but Go records the
// module version it fetched; a build from a checkout records "(devel)" and stays "dev".
func buildVersion(linked string, readBuildInfo func() (*debug.BuildInfo, bool)) string {
	if linked != "dev" {
		return linked
	}
	info, ok := readBuildInfo()
	if !ok {
		return linked
	}
	module := info.Main.Version
	if module == "" || module == "(devel)" {
		return linked
	}
	return strings.TrimPrefix(module, "v")
}

func newRootCommand(deps *command.Deps) *cobra.Command {
	root := command.NewRoot(deps)
	root.AddCommand(
		auth.New(deps),
		tenant.New(deps),
		catalog.NewCluster(deps),
		catalog.NewTemplate(deps),
		catalog.NewSize(deps),
		vm.New(deps),
		sshkey.New(deps),
		command.NewVersion(deps),
	)
	return root
}

func main() {
	endpoints := command.EndpointsFromEnv(os.Getenv)
	configDir, err := session.DefaultDir(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}
	deps := &command.Deps{
		Endpoints: endpoints,
		Sessions: &session.Manager{
			Endpoints: endpoints,
			Store:     session.Store{Dir: configDir},
			Keycloak: session.Keycloak{
				Issuer: endpoints.Auth,
				HTTP:   httpClient,
				Sleep:  time.Sleep,
				Now:    time.Now,
			},
		},
		Printer:     &output.Printer{Out: os.Stdout, Err: os.Stderr},
		HTTP:        httpClient,
		Version:     buildVersion(version, debug.ReadBuildInfo),
		OpenBrowser: command.OpenBrowser,
		Stdin:       os.Stdin,
		Executable:  os.Executable,
		ApiToken:    command.ApiTokenFromEnv(os.Getenv),
		RunSSH:      command.RunSSH,
		Clock:       wait.Clock{Now: time.Now, Sleep: time.Sleep},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err = newRootCommand(deps).ExecuteContext(ctx)
	stop()
	var exitCode *command.ExitCode
	if errors.As(err, &exitCode) {
		os.Exit(exitCode.Code)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
