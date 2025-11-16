package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/aliksend/terraform-provider-dokku/internal/config"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

var (
	_ resource.Resource                = &domainResource{}
	_ resource.ResourceWithConfigure   = &domainResource{}
	_ resource.ResourceWithImportState = &domainResource{}
)

func NewDomainResource() resource.Resource {
	return &domainResource{}
}

type domainResource struct {
	config *config.DokkuConfig
}

type domainResourceModel struct {
	Domain types.String `tfsdk:"domain"`
}

// Metadata returns the resource type name.
func (r *domainResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_domain"
}

// Configure adds the provider configured config to the resource.
func (r *domainResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	config, ok := req.ProviderData.(*config.DokkuConfig)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *config.DokkuConfig, got: %T", req.ProviderData),
		)
		return
	}

	r.config = config
}

// Schema defines the schema for the resource.
func (r *domainResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: strings.Join([]string{
			"For global domains setup",
			"For app-specific domains use app_resource.domains attribute",
			"https://dokku.com/docs/configuration/domains/",
		}, "\n  "),
		Attributes: map[string]schema.Attribute{
			"domain": schema.StringAttribute{
				Required:    true,
				Description: "Domain to use",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
		},
	}
}

// Read refreshes the Terraform state with the latest data.
func (r *domainResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	// Get current state
	var state domainResourceModel
	diags := req.State.Get(ctx, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Create SSH connection on-demand
	client, err := r.config.NewClient(ctx)
	if err != nil {
		if r.config.SkipUnreachableOnDestroy {
			tflog.Warn(ctx, "SSH connection failed during read, but skip_unreachable_on_destroy is enabled. Removing resource from state.", map[string]any{
				"resource": "dokku_domain",
				"error":    err.Error(),
			})
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("SSH connection failed", err.Error())
		return
	}
	defer r.config.CloseClient(client)

	// Read domains
	exists, err := client.GlobalDomainExists(ctx, state.Domain.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to check global domain existence", "Unable to check global domain existence. "+err.Error())
		return
	}
	if !exists {
		resp.State.RemoveResource(ctx)
		return
	}

	// Set refreshed state
	diags = resp.State.Set(ctx, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
}

// Create creates the resource and sets the initial Terraform state.
func (r *domainResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	// Retrieve values from plan
	var plan domainResourceModel
	diags := req.Plan.Get(ctx, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Create SSH connection on-demand
	client, err := r.config.NewClient(ctx)
	if err != nil {
		resp.Diagnostics.AddError("SSH connection failed", err.Error())
		return
	}
	defer r.config.CloseClient(client)

	// Read domains
	exists, err := client.GlobalDomainExists(ctx, plan.Domain.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to check global domain existence", "Unable to check global domain existence. "+err.Error())
		return
	}
	if exists {
		resp.Diagnostics.AddAttributeError(path.Root("domain"), "This global domain is already set", "This global domain is already set")
		return
	}

	// Add domain
	err = client.GlobalDomainAdd(ctx, plan.Domain.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to add global domain", "Unable to add global domain. "+err.Error())
		return
	}

	// Set state to fully populated data
	diags = resp.State.Set(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
}

// Update updates the resource and sets the updated Terraform state on success.
func (r *domainResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("Resource doesn't support Update", "Resource doesn't support Update")
}

// Delete deletes the resource and removes the Terraform state on success.
func (r *domainResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	// Retrieve values from state
	var state domainResourceModel
	diags := req.State.Get(ctx, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Create SSH connection on-demand
	client, err := r.config.NewClient(ctx)
	if err != nil {
		if r.config.SkipUnreachableOnDestroy {
			tflog.Warn(ctx, "SSH connection failed during destroy, but skip_unreachable_on_destroy is enabled. Removing resource from state without remote deletion.", map[string]any{
				"resource": "dokku_domain",
				"domain":   state.Domain.ValueString(),
				"error":    err.Error(),
			})
			return
		}
		resp.Diagnostics.AddError("SSH connection failed", err.Error())
		return
	}
	defer r.config.CloseClient(client)

	// Read domains
	exists, err := client.GlobalDomainExists(ctx, state.Domain.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to check global domain existence", "Unable to check global domain existence. "+err.Error())
		return
	}
	if !exists {
		return
	}

	// Clear domains
	err = client.GlobalDomainRemove(ctx, state.Domain.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to remove global domain", "Unable to remove global domain. "+err.Error())
		return
	}
}

func (r *domainResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Retrieve import ID and save to app_name attribute
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("domain"), req.ID)...)
}
