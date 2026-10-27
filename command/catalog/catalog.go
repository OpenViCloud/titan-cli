// Package catalog is what a customer chooses from when creating a VM: `cluster list`, `template list`, `size list`.
package catalog

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/OpenViCloud/titan-cli/api"
	"github.com/OpenViCloud/titan-cli/command"
	"github.com/OpenViCloud/titan-cli/output"
)

func NewCluster(deps *command.Deps) *cobra.Command {
	cluster := &cobra.Command{Use: "cluster", Short: "Clusters you can create resources in"}
	cluster.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List the clusters",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := deps.Client(cmd.Context())
			if err != nil {
				return err
			}
			response, err := client.TitanInfrastructureEndpointsCatalogListClustersEndpointWithResponse(cmd.Context())
			if err != nil {
				return err
			}
			if response.JSON200 == nil {
				return output.Explain(api.ProblemOf(response.HTTPResponse, response.Body))
			}
			rows := [][]string{}
			for _, cluster := range *response.JSON200 {
				rows = append(rows, []string{output.Text(cluster.Id), output.Text(cluster.Name)})
			}
			return deps.Printer.Table([]string{"ID", "NAME"}, rows, response.JSON200)
		},
	})
	return cluster
}

func NewTemplate(deps *command.Deps) *cobra.Command {
	var cluster string
	list := &cobra.Command{
		Use:   "list",
		Short: "List the operating systems a VM can start from",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := deps.Client(cmd.Context())
			if err != nil {
				return err
			}
			templates, err := Templates(cmd.Context(), client, cluster)
			if err != nil {
				return err
			}
			rows := [][]string{}
			for _, template := range templates {
				rows = append(rows, []string{output.Text(template.Id), output.Text(template.Name), output.Text(template.Os), output.Int(template.DiskGb)})
			}
			return deps.Printer.Table([]string{"ID", "NAME", "OS", "DISK GB"}, rows, templates)
		},
	}
	list.Flags().StringVar(&cluster, "cluster", "", "only the templates ready in this cluster")
	template := &cobra.Command{Use: "template", Short: "Operating systems for VMs"}
	template.AddCommand(list)
	return template
}

// Templates lists the templates a customer can pick, in one cluster when cluster is set.
func Templates(ctx context.Context, client *api.ClientWithResponses, cluster string) ([]api.TemplateQueryResult, error) {
	params := &api.TitanInfrastructureEndpointsCatalogListTemplatesEndpointParams{}
	if cluster != "" {
		params.ClusterId = &cluster
	}
	response, err := client.TitanInfrastructureEndpointsCatalogListTemplatesEndpointWithResponse(ctx, params)
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, output.Explain(api.ProblemOf(response.HTTPResponse, response.Body))
	}
	return *response.JSON200, nil
}

func NewSize(deps *command.Deps) *cobra.Command {
	size := &cobra.Command{Use: "size", Short: "How big a VM is, and its price"}
	size.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List the VM sizes and their price per hour (VND)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := deps.Client(cmd.Context())
			if err != nil {
				return err
			}
			sizes, err := Sizes(cmd.Context(), client)
			if err != nil {
				return err
			}
			rows := [][]string{}
			for _, size := range sizes {
				rows = append(rows, []string{output.Text(size.Id), output.Text(size.Name), output.Int(size.Vcpu),
					output.Number(size.Cpu), output.Int(size.MemoryMb), output.Number(size.PricePerHour)})
			}
			return deps.Printer.Table([]string{"ID", "NAME", "VCPU", "CPU", "MEMORY MB", "PRICE/HOUR"}, rows, sizes)
		},
	})
	return size
}

func Sizes(ctx context.Context, client *api.ClientWithResponses) ([]api.VmOfferingQueryResult, error) {
	response, err := client.TitanInfrastructureEndpointsCatalogListVmOfferingsEndpointWithResponse(ctx)
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, output.Explain(api.ProblemOf(response.HTTPResponse, response.Body))
	}
	return *response.JSON200, nil
}
