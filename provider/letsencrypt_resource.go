package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/DimmKirr/terraform-provider-dokku/internal/config"
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
	_ resource.Resource                = &letsencryptResource{}
	_ resource.ResourceWithConfigure   = &letsencryptResource{}
	_ resource.ResourceWithImportState = &letsencryptResource{}
)

func NewLetsencryptResource() resource.Resource {
	return &letsencryptResource{}
}

type letsencryptResource struct {
	config *config.DokkuConfig
}

type letsencryptResourceModel struct {
	AppName types.String `tfsdk:"app_name"`
	Email   types.String `tfsdk:"email"`
}

// Metadata returns the resource type name.
func (r *letsencryptResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_letsencrypt"
}

// Configure adds the provider configured config to the resource.
func (r *letsencryptResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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
func (r *letsencryptResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: strings.Join([]string{
			"lentsencrypt plugin support",
			"https://github.com/dokku/dokku-letsencrypt/",
		}, "\n  "),
		Attributes: map[string]schema.Attribute{
			"app_name": schema.StringAttribute{
				Required:    true,
				Description: "App name to apply letsencrypt to. Requires domain and ports to be set",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{
					stringvalidator.RegexMatches(regexp.MustCompile(`^[a-z][a-z0-9-]*$`), "invalid app_name"),
				},
			},
			"email": schema.StringAttribute{
				Required:    true,
				Description: "Email to use for letsencrypt",
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
		},
	}
}

// Read refreshes the Terraform state with the latest data.
func (r *letsencryptResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	// Get current state
	var state letsencryptResourceModel
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
				"resource": "dokku_letsencrypt",
				"error":    err.Error(),
			})
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("SSH connection failed", err.Error())
		return
	}
	defer r.config.CloseClient(client)

	// Read letsencrypt status
	exists, err := client.LetsencryptIsEnabled(ctx, state.AppName.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to read letsencrypt status", "Unable to read letsencrypt status. "+err.Error())
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
func (r *letsencryptResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	// Retrieve values from plan
	var plan letsencryptResourceModel
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

	// Read letsencrypt status
	exists, err := client.LetsencryptIsEnabled(ctx, plan.AppName.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to read letsencrypt status", "Unable to read letsencrypt status. "+err.Error())
		return
	}
	if exists {
		resp.Diagnostics.AddAttributeError(path.Root("app_name"), "Letsencrypt already enabled for app", "Letsencrypt already enabled for app")
		return
	}

	// Set letsencrypt email
	err = client.LetsencryptSetEmail(ctx, plan.AppName.ValueString(), plan.Email.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to set letsencrypt email", "Unable to set letsencrypt email. "+err.Error())
		return
	}

	// Enable letsencrypt
	err = client.LetsencryptEnable(ctx, plan.AppName.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to enable letsencrypt", "Unable to enable letsencrypt. "+err.Error())
		return
	}

	// Add cronjob for auto-renew
	err = client.LetsencryptAddCronJob(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Unable to add letsencrypt cronjob", "Unable to add letsencrypt cronjob. "+err.Error())
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
func (r *letsencryptResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// Retrieve values from plan
	var plan letsencryptResourceModel
	diags := req.Plan.Get(ctx, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	var state letsencryptResourceModel
	diags = req.State.Get(ctx, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	if plan.AppName.ValueString() != state.AppName.ValueString() {
		resp.Diagnostics.AddAttributeError(path.Root("app_name"), "App name can't be changed", "App name can't be changed")
		return
	}

	// Create SSH connection on-demand
	client, err := r.config.NewClient(ctx)
	if err != nil {
		resp.Diagnostics.AddError("SSH connection failed", err.Error())
		return
	}
	defer r.config.CloseClient(client)

	err = client.LetsencryptSetEmail(ctx, plan.AppName.ValueString(), plan.Email.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to set letsencrypt email", "Unable to set letsencrypt email. "+err.Error())
		return
	}

	diags = resp.State.Set(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
}

// Delete deletes the resource and removes the Terraform state on success.
func (r *letsencryptResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	// Retrieve values from state
	var state letsencryptResourceModel
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
				"resource": "dokku_letsencrypt",
				"app_name": state.AppName.ValueString(),
				"error":    err.Error(),
			})
			return
		}
		resp.Diagnostics.AddError("SSH connection failed", err.Error())
		return
	}
	defer r.config.CloseClient(client)

	// Read letsencrypt status
	exists, err := client.LetsencryptIsEnabled(ctx, state.AppName.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to read letsencrypt status", "Unable to read letsencrypt status. "+err.Error())
		return
	}
	if !exists {
		return
	}

	// Disable letsencrypt
	err = client.LetsencryptDisable(ctx, state.AppName.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to disable letsencrypt", "Unable to disable letsencrypt. "+err.Error())
		return
	}
}

func (r *letsencryptResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Retrieve import ID and save to app_name attribute
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("app_name"), req.ID)...)
}
