package provider

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/aliksend/terraform-provider-dokku/internal/config"
	dokkuclient "github.com/aliksend/terraform-provider-dokku/provider/dokku_client"

	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

var (
	_ resource.Resource                   = &appResource{}
	_ resource.ResourceWithConfigure      = &appResource{}
	_ resource.ResourceWithImportState    = &appResource{}
	_ resource.ResourceWithValidateConfig = &appResource{}
)

// storageObjectType defines the object type for storage map elements
var storageObjectType = types.ObjectType{
	AttrTypes: map[string]attr.Type{
		"local_directory": types.StringType,
		"mount_path":      types.StringType,
	},
}

// dockerOptionObjectType defines the object type for docker_options map elements
var dockerOptionObjectType = types.ObjectType{
	AttrTypes: map[string]attr.Type{
		"phase": types.SetType{ElemType: types.StringType},
	},
}

func NewAppResource() resource.Resource {
	return &appResource{}
}

type appResource struct {
	config *config.DokkuConfig
}

type appResourceModel struct {
	AppName       types.String         `tfsdk:"app_name"`
	Config        types.Map            `tfsdk:"config"`
	Storage       types.Map            `tfsdk:"storage"`
	Checks        *checkModel          `tfsdk:"checks"`
	Ports         map[string]portModel `tfsdk:"ports"`
	ProxyPorts    map[string]portModel `tfsdk:"proxy_ports"`
	Domains       types.Set            `tfsdk:"domains"`
	DockerOptions types.Map            `tfsdk:"docker_options"`
	Networks      *networkModel        `tfsdk:"networks"`
	Deploy        *deployModel         `tfsdk:"deploy"`
}

type storageModel struct {
	LocalDirectory types.String `tfsdk:"local_directory"`
	MountPath      types.String `tfsdk:"mount_path"`
}

type checkModel struct {
	Status types.String `tfsdk:"status"`
}

type portModel struct {
	Scheme        types.String `tfsdk:"scheme"`
	ContainerPort types.String `tfsdk:"container_port"`
}

type dockerOptionModel struct {
	Phase types.Set `tfsdk:"phase"`
}

type networkModel struct {
	AttachPostCreate types.String `tfsdk:"attach_post_create"`
	AttachPostDeploy types.String `tfsdk:"attach_post_deploy"`
	InitialNetwork   types.String `tfsdk:"initial_network"`
}

type deployModel struct {
	Type             types.String `tfsdk:"type"`
	Login            types.String `tfsdk:"login"`
	Password         types.String `tfsdk:"password"`
	DockerImage      types.String `tfsdk:"docker_image"`
	AllowRebuild     types.Bool   `tfsdk:"allow_rebuild"`
	GitRepository    types.String `tfsdk:"git_repository"`
	GitRepositoryRef types.String `tfsdk:"git_repository_ref"`
	ArchiveType      types.String `tfsdk:"archive_type"`
	ArchiveUrl       types.String `tfsdk:"archive_url"`
}

// Metadata returns the resource type name.
func (r *appResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_app"
}

// Configure adds the provider configured config to the resource.
func (r *appResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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
func (r *appResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: strings.Join([]string{
			"dokku app",
			"https://dokku.com/docs/deployment/application-management/",
		}, "\n  "),
		Attributes: map[string]schema.Attribute{
			"app_name": schema.StringAttribute{
				Required:    true,
				Description: "Name of application to manage",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{
					stringvalidator.RegexMatches(regexp.MustCompile(`^[a-z][a-z0-9-]*$`), "invalid app_name"),
				},
			},
			"config": schema.MapAttribute{
				Optional:    true,
				Description: "Config (env vars) for app",
				ElementType: types.StringType,
				Validators: []validator.Map{
					mapvalidator.KeysAre(stringvalidator.RegexMatches(regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`), "invalid name")),
					mapvalidator.ValueStringsAre(stringvalidator.LengthAtLeast(1)),
				},
			},
			"storage": schema.MapNestedAttribute{
				Optional:    true,
				Description: "Persistent storage setup for app. Keys are storage names or absolute paths to host directories",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"local_directory": schema.StringAttribute{
							Optional: true,
							Description: strings.Join([]string{
								"Uploads local directory to host (always, without checking is it changed)",
								"",
								"Should not be used for uploading large files, because it is slow.",
								"Also see upload_* attributes in provider configuration.",
							}, "\n  "),
							Validators: []validator.String{
								stringvalidator.LengthAtLeast(1),
							},
						},
						// Improvements:
						// Calculate checksum of files on remote host on Read. Upload local files on Update only if checksum changed
						// - calculate only for directories with set local_directory to prevent processing large storages
						"mount_path": schema.StringAttribute{
							Required:    true,
							Description: "Path inside container to mount to",
							Validators: []validator.String{
								stringvalidator.LengthAtLeast(1),
							},
						},
					},
				},
				Validators: []validator.Map{
					mapvalidator.KeysAre(stringvalidator.LengthAtLeast(1)),
				},
			},
			"checks": schema.SingleNestedAttribute{
				Optional:    true,
				Description: "Checks setup for app",
				Attributes: map[string]schema.Attribute{
					"status": schema.StringAttribute{
						Required:    true,
						Description: "Checks status. Default: enabled",
						Validators: []validator.String{
							stringvalidator.OneOf("enabled", "disabled", "skipped"),
						},
					},
				},
			},
			"ports": schema.MapNestedAttribute{
				Optional:    true,
				Description: "Ports setup for app. Keys are host ports",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"scheme": schema.StringAttribute{
							Required:    true,
							Description: "Scheme to use. Allowed values: http, https",
							Validators: []validator.String{
								stringvalidator.OneOf("http", "https"),
							},
						},
						"container_port": schema.StringAttribute{
							Required:    true,
							Description: "Port inside container to proxy",
							Validators: []validator.String{
								stringvalidator.RegexMatches(regexp.MustCompile(`^\d+$`), "Must be integer"),
							},
						},
					},
				},
				Validators: []validator.Map{
					mapvalidator.KeysAre(stringvalidator.RegexMatches(regexp.MustCompile(`^\d+$`), "Must be integer")),
				},
			},
			"proxy_ports": schema.MapNestedAttribute{
				Optional:    true,
				Description: "DEPRECATED. Use \"ports\" instead.\n\nProxy ports setup for app. Keys are host ports.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"scheme": schema.StringAttribute{
							Required:    true,
							Description: "Scheme to use. Allowed values: http, https",
							Validators: []validator.String{
								stringvalidator.OneOf("http", "https"),
							},
						},
						"container_port": schema.StringAttribute{
							Required:    true,
							Description: "Port inside container to proxy",
							Validators: []validator.String{
								stringvalidator.RegexMatches(regexp.MustCompile(`^\d+$`), "Must be integer"),
							},
						},
					},
				},
				Validators: []validator.Map{
					mapvalidator.KeysAre(stringvalidator.RegexMatches(regexp.MustCompile(`^\d+$`), "Must be integer")),
				},
			},
			"domains": schema.SetAttribute{
				Optional:    true,
				Description: "Domains setup for app",
				ElementType: types.StringType,
				Validators: []validator.Set{
					setvalidator.ValueStringsAre(stringvalidator.LengthAtLeast(1)),
				},
			},
			"docker_options": schema.MapNestedAttribute{
				Optional:    true,
				Description: "Docker options for app. Keys are options",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"phase": schema.SetAttribute{
							Required:    true,
							Description: "Phase to apply docker-options to. Allowed values: build, deploy, run",
							ElementType: types.StringType,
							Validators: []validator.Set{
								setvalidator.ValueStringsAre(stringvalidator.OneOf("build", "deploy", "run")),
							},
						},
					},
				},
				Validators: []validator.Map{
					mapvalidator.KeysAre(stringvalidator.LengthAtLeast(1)),
				},
			},
			"networks": schema.SingleNestedAttribute{
				Optional:    true,
				Description: "Network setup for app",
				Attributes: map[string]schema.Attribute{
					"attach_post_create": schema.StringAttribute{
						Optional:    true,
						Description: "Name of network to use as attach-post-create",
						Validators: []validator.String{
							stringvalidator.LengthAtLeast(1),
						},
					},
					"attach_post_deploy": schema.StringAttribute{
						Optional:    true,
						Description: "Name of network to use as attach-post-deploy",
						Validators: []validator.String{
							stringvalidator.LengthAtLeast(1),
						},
					},
					"initial_network": schema.StringAttribute{
						Optional:    true,
						Description: "Name of network to use as initial-network",
						Validators: []validator.String{
							stringvalidator.LengthAtLeast(1),
						},
					},
				},
			},
			"deploy": schema.SingleNestedAttribute{
				Optional:    true,
				Description: "Deploy setup for app",
				Attributes: map[string]schema.Attribute{
					"type": schema.StringAttribute{
						Required:    true,
						Description: "Type of deploy to use. Allowed values: archive, docker_image, git_repository",
						Validators: []validator.String{
							stringvalidator.OneOf("archive", "docker_image", "git_repository"),
						},
					},
					"login": schema.StringAttribute{
						Optional:    true,
						Description: "Login to use for deployment",
						Validators: []validator.String{
							stringvalidator.LengthAtLeast(1),
							stringvalidator.AlsoRequires(path.MatchRelative().AtParent().AtName("password")),
						},
					},
					"password": schema.StringAttribute{
						Optional:    true,
						Sensitive:   true,
						Description: "Password to use for deployment",
						Validators: []validator.String{
							stringvalidator.LengthAtLeast(1),
							stringvalidator.AlsoRequires(path.MatchRelative().AtParent().AtName("login")),
						},
					},
					"docker_image": schema.StringAttribute{
						Optional:    true,
						Description: "Docker image to deploy from. If login and password is provided then it will be used to sign in to docker registry.",
						Validators: []validator.String{
							stringvalidator.LengthAtLeast(1),
						},
					},
					"allow_rebuild": schema.BoolAttribute{
						Optional:    true,
						Description: "Allow to run ps:rebuild for app if same docker_image provided second time",
					},
					"git_repository": schema.StringAttribute{
						Optional:    true,
						Description: "Git repository to deploy from. If login and password is provided then it will be used to sign in to repository.",
						Validators: []validator.String{
							stringvalidator.LengthAtLeast(1),
						},
					},
					"git_repository_ref": schema.StringAttribute{
						Optional:    true,
						Description: "Ref of git repository to deploy from",
						Validators: []validator.String{
							stringvalidator.LengthAtLeast(1),
						},
					},
					"archive_url": schema.StringAttribute{
						Optional:    true,
						Description: "URL of archive to delpoy from. Login and password will not be used",
						Validators: []validator.String{
							stringvalidator.LengthAtLeast(1),
						},
					},
					"archive_type": schema.StringAttribute{
						Optional:    true,
						Description: "Type of archive to deploy. Allowed values: tar, tar.gz, zip", // https://github.com/dokku/dokku/blob/master/plugins/git/git-from-archive#L25
						Validators: []validator.String{
							stringvalidator.OneOf("tar", "tar.gz", "zip"),
						},
					},
				},
			},
		},
	}
}
func (r *appResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var data appResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if data.Deploy != nil {
		switch data.Deploy.Type.ValueString() {
		case "archive":
			if data.Deploy.ArchiveUrl.IsNull() {
				resp.Diagnostics.AddAttributeError(path.Root("deploy").AtName("archive_url"), "archive_url must be set for type archive", "archive_url must be set for type archive")
			}
		case "docker_image":
			if data.Deploy.DockerImage.IsNull() {
				resp.Diagnostics.AddAttributeError(path.Root("deploy").AtName("docker_image"), "docker_image must be set for type archive", "docker_image must be set for type archive")
			}
		case "git_repository":
			if data.Deploy.GitRepository.IsNull() {
				resp.Diagnostics.AddAttributeError(path.Root("deploy").AtName("git_repository"), "git_repository must be set for type archive", "git_repository must be set for type archive")
			}
		default:
			resp.Diagnostics.AddAttributeError(path.Root("deploy").AtName("type"), "Invalid type value", "Invalid type value")
		}
	}
}

// Read refreshes the Terraform state with the latest data.
func (r *appResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	// Get current state
	var state appResourceModel
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
				"resource": "dokku_app",
				"app_name": state.AppName.ValueString(),
				"error":    err.Error(),
			})
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("SSH connection failed", err.Error())
		return
	}
	defer r.config.CloseClient(client)

	// Check app existence
	exists, err := client.AppExists(ctx, state.AppName.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("app_name"), "Unable to check app existence", "Unable to check app existence. "+err.Error())
		return
	}
	if !exists {
		resp.State.RemoveResource(ctx)
		return
	}

	config, err := client.ConfigExport(ctx, state.AppName.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("config"), "Unable to get config", "Unable to get config. "+err.Error())
	} else {
		cfg := make(map[string]basetypes.StringValue)

		// Get existing config keys from state to filter
		var existingKeys []string
		if !state.Config.IsNull() && !state.Config.IsUnknown() {
			configElements := state.Config.Elements()
			for k := range configElements {
				existingKeys = append(existingKeys, k)
			}
		}

		for k, v := range config {
			found := false
			for _, knownK := range existingKeys {
				if k == knownK {
					found = true
					break
				}
			}
			// only known keys
			if found {
				cfg[k] = basetypes.NewStringValue(v)
			}
		}

		if len(cfg) == 0 {
			state.Config = basetypes.NewMapNull(types.StringType)
		} else {
			// Convert to map[string]attr.Value for basetypes.NewMapValue
			attrMap := make(map[string]attr.Value)
			for k, v := range cfg {
				attrMap[k] = v
			}
			mapVal, diags := basetypes.NewMapValue(types.StringType, attrMap)
			resp.Diagnostics.Append(diags...)
			state.Config = mapVal
		}
	}

	storage, err := client.StorageExport(ctx, state.AppName.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("storage"), "Unable to get storage", "Unable to get storage. "+err.Error())
	} else {
		if len(storage) == 0 {
			state.Storage = basetypes.NewMapNull(storageObjectType)
		} else {
			// Create map of storage objects
			attrMap := make(map[string]attr.Value)
			for k, v := range storage {
				localDirectory := basetypes.NewStringNull()

				// Get existing local_directory from state if it exists
				if !state.Storage.IsNull() && !state.Storage.IsUnknown() {
					storageElements := state.Storage.Elements()
					if existingElem, exists := storageElements[k]; exists {
						if objVal, ok := existingElem.(basetypes.ObjectValue); ok {
							attrs := objVal.Attributes()
							if localDirAttr, exists := attrs["local_directory"]; exists {
								if strVal, ok := localDirAttr.(basetypes.StringValue); ok {
									localDirectory = strVal
								}
							}
						}
					}
				}

				objVal, diags := basetypes.NewObjectValue(storageObjectType.AttrTypes, map[string]attr.Value{
					"local_directory": localDirectory,
					"mount_path":      basetypes.NewStringValue(v),
				})
				if diags.HasError() {
					resp.Diagnostics.Append(diags...)
					continue
				}
				attrMap[k] = objVal
			}

			mapVal, diags := basetypes.NewMapValue(storageObjectType, attrMap)
			resp.Diagnostics.Append(diags...)
			state.Storage = mapVal
		}
	}

	checkStatus, err := client.ChecksGet(ctx, state.AppName.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("checks"), "Unable to get checks", "Unable to get checks. "+err.Error())
	} else {
		if checkStatus == "enabled" {
			state.Checks = nil
		} else {
			state.Checks = &checkModel{
				Status: basetypes.NewStringValue(checkStatus),
			}
		}
	}

	domains, err := client.DomainsExport(ctx, state.AppName.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("domains"), "Unable to get domains", "Unable to get domains. "+err.Error())
	} else {
		if len(domains) == 0 {
			state.Domains = basetypes.NewSetNull(types.StringType)
		} else {
			// Convert to []attr.Value for basetypes.NewSetValue
			domainAttrs := make([]attr.Value, len(domains))
			for i, d := range domains {
				domainAttrs[i] = basetypes.NewStringValue(d)
			}
			setVal, diags := basetypes.NewSetValue(types.StringType, domainAttrs)
			resp.Diagnostics.Append(diags...)
			state.Domains = setVal
		}
	}

	ports, err := client.PortsExport(ctx, state.AppName.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("ports"), "Unable to get ports", "Unable to get ports. "+err.Error())
	} else {
		pp := make(map[string]portModel)
		for _, p := range ports {
			found := false
			for v := range state.Ports {
				if v == p.HostPort {
					found = true
					break
				}
			}
			for v := range state.ProxyPorts {
				if v == p.HostPort {
					found = true
					break
				}
			}
			// only known hostport's
			if found {
				pp[p.HostPort] = portModel{
					Scheme:        basetypes.NewStringValue(p.Scheme),
					ContainerPort: basetypes.NewStringValue(p.ContainerPort),
				}
			}
		}
		if len(pp) == 0 {
			state.Ports = nil
			state.ProxyPorts = nil
		} else {
			state.Ports = pp
			// Don't set state.ProxyPorts and don't check it later - use only state.Ports
			// state.ProxyPorts = pp
			state.ProxyPorts = nil
		}
	}

	// dockerOptions -- unable to read because it can be set externally

	networks, err := client.NetworksReport(ctx, state.AppName.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("networks"), "Unable to get networks", "Unable to get networks. "+err.Error())
	} else {
		var attachPostCreate types.String
		if networks["attach post create"] != "" {
			attachPostCreate = basetypes.NewStringValue(networks["attach post create"])
		} else {
			attachPostCreate = basetypes.NewStringNull()
		}
		var attachPostDeploy types.String
		if networks["attach post deploy"] != "" {
			attachPostDeploy = basetypes.NewStringValue(networks["attach post deploy"])
		} else {
			attachPostDeploy = basetypes.NewStringNull()
		}
		var initialNetwork types.String
		if networks["initial network"] != "" {
			initialNetwork = basetypes.NewStringValue(networks["initial network"])
		} else {
			initialNetwork = basetypes.NewStringNull()
		}
		if attachPostCreate.IsNull() && attachPostDeploy.IsNull() && initialNetwork.IsNull() {
			state.Networks = nil
		} else {
			state.Networks = &networkModel{
				AttachPostCreate: attachPostCreate,
				AttachPostDeploy: attachPostDeploy,
				InitialNetwork:   initialNetwork,
			}
		}
	}

	// deploy -- unable to read

	if resp.Diagnostics.HasError() {
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
func (r *appResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	// Retrieve values from plan
	var plan appResourceModel
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

	// Check app existence
	exists, err := client.AppExists(ctx, plan.AppName.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("app_name"), "Unable to check app existence", "Unable to check app existence. "+err.Error())
		return
	}
	if exists {
		resp.Diagnostics.AddAttributeError(path.Root("app_name"), "App already exists", "App with specified name already exists")
		return
	}

	// Create new app
	err = client.AppCreate(ctx, plan.AppName.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to create app", "Unable to create app. "+err.Error())
		// if not created - return to not try to destroy on other errors
		return
	}

	if !plan.Config.IsNull() && !plan.Config.IsUnknown() {
		config := make(map[string]string)
		configElements := plan.Config.Elements()
		for k, v := range configElements {
			if stringVal, ok := v.(basetypes.StringValue); ok {
				config[k] = stringVal.ValueString()
			}
		}
		err := client.ConfigSet(ctx, plan.AppName.ValueString(), config)
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("config"), "Unable to set config", "Unable to set config. "+err.Error())
		}
	}

	if !plan.Storage.IsNull() && !plan.Storage.IsUnknown() {
		storageElements := plan.Storage.Elements()
		for hostPath, elem := range storageElements {
			if objVal, ok := elem.(basetypes.ObjectValue); ok {
				attrs := objVal.Attributes()

				var localDirectoryPtr *string
				if localDirAttr, exists := attrs["local_directory"]; exists {
					if localDirStr, ok := localDirAttr.(basetypes.StringValue); ok && !localDirStr.IsNull() {
						val := localDirStr.ValueString()
						localDirectoryPtr = &val
					}
				}

				var mountPath string
				if mountPathAttr, exists := attrs["mount_path"]; exists {
					if mountPathStr, ok := mountPathAttr.(basetypes.StringValue); ok {
						mountPath = mountPathStr.ValueString()
					}
				}

				err := client.StorageEnsure(ctx, hostPath, localDirectoryPtr)
				if err != nil {
					resp.Diagnostics.AddAttributeError(path.Root("storage").AtMapKey(hostPath), "Unable to ensure storage", "Unable to ensure storage. "+err.Error())
				}

				err = client.StorageMount(ctx, plan.AppName.ValueString(), hostPath, mountPath)
				if err != nil {
					resp.Diagnostics.AddAttributeError(path.Root("storage").AtMapKey(hostPath), "Unable to mount storage", "Unable to mount storage. "+err.Error())
				}
			}
		}
	}

	if plan.Checks != nil {
		if !plan.Checks.Status.IsNull() {
			err := client.ChecksSet(ctx, plan.AppName.ValueString(), plan.Checks.Status.ValueString())
			if err != nil {
				resp.Diagnostics.AddAttributeError(path.Root("checks"), "Unable to set checks", "Unable to set checks. "+err.Error())
			}
		}
	}

	if len(plan.Ports) != 0 || len(plan.ProxyPorts) != 0 {
		if len(plan.ProxyPorts) > 0 {
			resp.Diagnostics.AddAttributeWarning(path.Root("proxy_ports"), "proxy_ports attribute is deprecated, use ports attribute instead", "proxy_ports attribute is deprecated, use ports attribute instead")
		}

		var ports []dokkuclient.Port
		for hostPort, port := range plan.Ports {
			ports = append(ports, dokkuclient.Port{
				Scheme:        port.Scheme.ValueString(),
				HostPort:      hostPort,
				ContainerPort: port.ContainerPort.ValueString(),
			})
		}
		for hostPort, port := range plan.ProxyPorts {
			ports = append(ports, dokkuclient.Port{
				Scheme:        port.Scheme.ValueString(),
				HostPort:      hostPort,
				ContainerPort: port.ContainerPort.ValueString(),
			})
		}
		err := client.PortsSet(ctx, plan.AppName.ValueString(), ports)
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("ports"), "Unable to set ports", "Unable to set ports. "+err.Error())
		}
		err = client.ProxyEnable(ctx, plan.AppName.ValueString())
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("ports"), "Unable to enable ports", "Unable to enable ports. "+err.Error())
		}
	} else {
		err = client.ProxyDisable(ctx, plan.AppName.ValueString())
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("ports"), "Unable to disable ports", "Unable to disable ports. "+err.Error())
		}
	}

	if !plan.Domains.IsNull() && !plan.Domains.IsUnknown() {
		var domains []string
		domainElements := plan.Domains.Elements()
		for _, domainVal := range domainElements {
			if stringVal, ok := domainVal.(basetypes.StringValue); ok {
				domains = append(domains, stringVal.ValueString())
			}
		}
		err := client.DomainsSet(ctx, plan.AppName.ValueString(), domains)
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("domains"), "Unable to add domain", "Unable to add domain. "+err.Error())
		}
		err = client.DomainsEnable(ctx, plan.AppName.ValueString())
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("domains"), "Unable to enable domains support", "Unable to enable domains support. "+err.Error())
		}
	} else {
		err = client.DomainsDisable(ctx, plan.AppName.ValueString())
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("domains"), "Unable to disable domains support", "Unable to disable domains support. "+err.Error())
		}
	}

	if !plan.DockerOptions.IsNull() && !plan.DockerOptions.IsUnknown() {
		dockerOptionsElements := plan.DockerOptions.Elements()
		for option, dockerOptionValue := range dockerOptionsElements {
			if objVal, ok := dockerOptionValue.(basetypes.ObjectValue); ok {
				attrs := objVal.Attributes()
				if phaseAttr, exists := attrs["phase"]; exists {
					if phaseSet, ok := phaseAttr.(basetypes.SetValue); ok {
						err := client.DockerOptionAdd(ctx, plan.AppName.ValueString(), formatDockerOptionsPhases(phaseSet), option)
						if err != nil {
							resp.Diagnostics.AddAttributeError(path.Root("docker_options").AtMapKey(option), "Unable to add docker option", "Unable to add docker option. "+err.Error())
						}
					}
				}
			}
		}
	}

	if plan.Networks != nil {
		if !plan.Networks.AttachPostCreate.IsNull() {
			err := client.NetworkEnsureAndSetForApp(ctx, plan.AppName.ValueString(), "attach-post-create", plan.Networks.AttachPostCreate.ValueString())
			if err != nil {
				resp.Diagnostics.AddAttributeError(path.Root("networks").AtName("attach_post_create"), "Unable to set network", "Unable to set network. "+err.Error())
			}
		}
		if !plan.Networks.AttachPostDeploy.IsNull() {
			err := client.NetworkEnsureAndSetForApp(ctx, plan.AppName.ValueString(), "attach-post-deploy", plan.Networks.AttachPostDeploy.ValueString())
			if err != nil {
				resp.Diagnostics.AddAttributeError(path.Root("networks").AtName("attach_post_deploy"), "Unable to set network", "Unable to set network. "+err.Error())
			}
		}
		if !plan.Networks.InitialNetwork.IsNull() {
			err := client.NetworkEnsureAndSetForApp(ctx, plan.AppName.ValueString(), "initial_network", plan.Networks.InitialNetwork.ValueString())
			if err != nil {
				resp.Diagnostics.AddAttributeError(path.Root("networks").AtName("initial_network"), "Unable to set network", "Unable to set network. "+err.Error())
			}
		}
	}

	if plan.Deploy != nil && !resp.Diagnostics.HasError() {
		_, err := r.deploy(ctx, client, plan.AppName.ValueString(), *plan.Deploy)
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("deploy"), "Unable to deploy", "Unable to deploy. "+err.Error())
		}
	}

	if resp.Diagnostics.HasError() {
		err := client.AppDestroy(ctx, plan.AppName.ValueString())
		if err != nil {
			resp.Diagnostics.AddError("Unable to destroy app", "Unable to destroy app. "+err.Error())
		}
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
func (r *appResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// Retrieve values from plan
	var plan appResourceModel
	diags := req.Plan.Get(ctx, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	var state appResourceModel
	diags = req.State.Get(ctx, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	if plan.AppName.ValueString() != state.AppName.ValueString() {
		resp.Diagnostics.AddAttributeError(path.Root("app_name"), "App name can't be changed", "App name can't be changed")
		return
	}
	appName := plan.AppName.ValueString()

	// Create SSH connection on-demand
	client, err := r.config.NewClient(ctx)
	if err != nil {
		resp.Diagnostics.AddError("SSH connection failed", err.Error())
		return
	}
	defer r.config.CloseClient(client)

	restartRequired := false

	// -- config
	var namesToUnset []string
	var stateConfigElements, planConfigElements map[string]basetypes.StringValue

	// Get state config elements
	if !state.Config.IsNull() && !state.Config.IsUnknown() {
		stateConfigElements = make(map[string]basetypes.StringValue)
		for k, v := range state.Config.Elements() {
			if stringVal, ok := v.(basetypes.StringValue); ok {
				stateConfigElements[k] = stringVal
			}
		}
	}

	// Get plan config elements
	if !plan.Config.IsNull() && !plan.Config.IsUnknown() {
		planConfigElements = make(map[string]basetypes.StringValue)
		for k, v := range plan.Config.Elements() {
			if stringVal, ok := v.(basetypes.StringValue); ok {
				planConfigElements[k] = stringVal
			}
		}
	}

	// Find keys to unset (in state but not in plan)
	for stateName := range stateConfigElements {
		found := false
		for planName := range planConfigElements {
			if planName == stateName {
				found = true
				break
			}
		}
		if !found {
			namesToUnset = append(namesToUnset, stateName)
		}
	}
	if len(namesToUnset) != 0 {
		err := client.ConfigUnset(ctx, appName, namesToUnset)
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("config"), "Unable to unset config", "Unable to unset config. "+err.Error())
		}
		restartRequired = true
	}

	// Find keys to set (new or changed values)
	configToSet := make(map[string]string)
	for k, v := range planConfigElements {
		stateVal, exists := stateConfigElements[k]
		if !exists || !stateVal.Equal(v) {
			configToSet[k] = v.ValueString()
		}
	}
	if len(configToSet) != 0 {
		err := client.ConfigSet(ctx, appName, configToSet)
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("config"), "Unable to set config", "Unable to set config. "+err.Error())
		}
		restartRequired = true
	}
	// --

	// -- storage
	// Get state storage elements
	var stateStorageElements map[string]attr.Value
	if !state.Storage.IsNull() && !state.Storage.IsUnknown() {
		stateStorageElements = state.Storage.Elements()
	}

	// Get plan storage elements
	var planStorageElements map[string]attr.Value
	if !plan.Storage.IsNull() && !plan.Storage.IsUnknown() {
		planStorageElements = plan.Storage.Elements()
	}

	// Handle removals (in state but not in plan)
	for existingName, existingElem := range stateStorageElements {
		_, found := planStorageElements[existingName]
		if !found {
			if objVal, ok := existingElem.(basetypes.ObjectValue); ok {
				attrs := objVal.Attributes()

				var mountPath string
				if mountPathAttr, exists := attrs["mount_path"]; exists {
					if mountPathStr, ok := mountPathAttr.(basetypes.StringValue); ok {
						mountPath = mountPathStr.ValueString()
					}
				}

				err := client.StorageUnmount(ctx, appName, existingName, mountPath)
				if err != nil {
					resp.Diagnostics.AddAttributeError(path.Root("storage").AtMapKey(existingName), "Unable to unmount storage", "Unable to unmount storage. "+err.Error())
				}
				restartRequired = true
			}
		}
	}

	// Handle updates and additions
	for planName, planElem := range planStorageElements {
		if planObjVal, ok := planElem.(basetypes.ObjectValue); ok {
			planAttrs := planObjVal.Attributes()

			var planLocalDirectoryPtr *string
			if localDirAttr, exists := planAttrs["local_directory"]; exists {
				if localDirStr, ok := localDirAttr.(basetypes.StringValue); ok && !localDirStr.IsNull() {
					val := localDirStr.ValueString()
					planLocalDirectoryPtr = &val
				}
			}

			var planMountPath string
			if mountPathAttr, exists := planAttrs["mount_path"]; exists {
				if mountPathStr, ok := mountPathAttr.(basetypes.StringValue); ok {
					planMountPath = mountPathStr.ValueString()
				}
			}

			if existingElem, exists := stateStorageElements[planName]; exists {
				// Update existing storage
				if existingObjVal, ok := existingElem.(basetypes.ObjectValue); ok {
					existingAttrs := existingObjVal.Attributes()

					var existingMountPath string
					if mountPathAttr, exists := existingAttrs["mount_path"]; exists {
						if mountPathStr, ok := mountPathAttr.(basetypes.StringValue); ok {
							existingMountPath = mountPathStr.ValueString()
						}
					}

					// Check if mount path changed
					if existingMountPath != planMountPath {
						err := client.StorageUnmount(ctx, appName, planName, existingMountPath)
						if err != nil {
							resp.Diagnostics.AddAttributeError(path.Root("storage").AtMapKey(planName), "Unable to unmount storage", "Unable to unmount storage. "+err.Error())
						}

						err = client.StorageEnsure(ctx, planName, planLocalDirectoryPtr)
						if err != nil {
							resp.Diagnostics.AddAttributeError(path.Root("storage").AtMapKey(planName), "Unable to ensure storage", "Unable to ensure storage. "+err.Error())
						}

						err = client.StorageMount(ctx, appName, planName, planMountPath)
						if err != nil {
							resp.Diagnostics.AddAttributeError(path.Root("storage").AtMapKey(planName), "Unable to mount storage", "Unable to mount storage. "+err.Error())
						}

						restartRequired = true
					} else if planLocalDirectoryPtr != nil {
						err := client.StorageEnsure(ctx, planName, planLocalDirectoryPtr)
						if err != nil {
							resp.Diagnostics.AddAttributeError(path.Root("storage").AtMapKey(planName), "Unable to ensure storage", "Unable to ensure storage. "+err.Error())
						}

						restartRequired = true
					}
				}
			} else {
				// Add new storage
				err := client.StorageEnsure(ctx, planName, planLocalDirectoryPtr)
				if err != nil {
					resp.Diagnostics.AddAttributeError(path.Root("storage").AtMapKey(planName), "Unable to ensure storage", "Unable to ensure storage. "+err.Error())
				}

				err = client.StorageMount(ctx, appName, planName, planMountPath)
				if err != nil {
					resp.Diagnostics.AddAttributeError(path.Root("storage").AtMapKey(planName), "Unable to mount storage", "Unable to mount storage. "+err.Error())
				}

				restartRequired = true
			}
		}
	}
	// --

	// -- checks
	stateCheckStatus := "enabled"
	if state.Checks != nil {
		stateCheckStatus = state.Checks.Status.ValueString()
	}
	planCheckStatus := "enabled"
	if plan.Checks != nil {
		planCheckStatus = plan.Checks.Status.ValueString()
	}
	if stateCheckStatus != planCheckStatus {
		err := client.ChecksSet(ctx, appName, planCheckStatus)
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("checks"), "Unable to set checks", "Unable to set checks. "+err.Error())
		}
	}
	// --

	// -- ports
	if len(plan.ProxyPorts) > 0 {
		resp.Diagnostics.AddAttributeWarning(path.Root("proxy_ports"), "proxy_ports attribute is deprecated, use ports attribute instead", "proxy_ports attribute is deprecated, use ports attribute instead")
	}
	needToSetPorts := false
	var portsToSet []dokkuclient.Port
	for existingHostPort, existingPort := range state.Ports {
		found := false
		for planHostPort, planPort := range plan.Ports {
			if planHostPort == existingHostPort {
				if planPort.Scheme.Equal(existingPort.Scheme) && planPort.ContainerPort.Equal(existingPort.ContainerPort) {
					found = true
				}
				break
			}
		}
		for planHostPort, planPort := range plan.ProxyPorts {
			if planHostPort == existingHostPort {
				if planPort.Scheme.Equal(existingPort.Scheme) && planPort.ContainerPort.Equal(existingPort.ContainerPort) {
					found = true
				}
				break
			}
		}
		if !found {
			needToSetPorts = true
		}
	}
	for planHostPort, planPort := range plan.Ports {
		found := false
		for existingHostPort, existingPort := range state.Ports {
			if planHostPort == existingHostPort {
				if planPort.Scheme.Equal(existingPort.Scheme) && planPort.ContainerPort.Equal(existingPort.ContainerPort) {
					found = true
				}
				break
			}
		}
		if !found {
			needToSetPorts = true
		}
		portsToSet = append(portsToSet, dokkuclient.Port{
			Scheme:        planPort.Scheme.ValueString(),
			HostPort:      planHostPort,
			ContainerPort: planPort.ContainerPort.ValueString(),
		})
	}
	for planHostPort, planPort := range plan.ProxyPorts {
		found := false
		for existingHostPort, existingPort := range state.Ports {
			if planHostPort == existingHostPort {
				if planPort.Scheme.Equal(existingPort.Scheme) && planPort.ContainerPort.Equal(existingPort.ContainerPort) {
					found = true
				}
				break
			}
		}
		if !found {
			needToSetPorts = true
		}
		portsToSet = append(portsToSet, dokkuclient.Port{
			Scheme:        planPort.Scheme.ValueString(),
			HostPort:      planHostPort,
			ContainerPort: planPort.ContainerPort.ValueString(),
		})
	}
	if needToSetPorts {
		if len(portsToSet) == 0 {
			err := client.PortsClear(ctx, appName)
			if err != nil {
				resp.Diagnostics.AddAttributeError(path.Root("ports"), "Unable to clear ports", "Unable to clear ports. "+err.Error())
			}

			err = client.ProxyDisable(ctx, appName)
			if err != nil {
				resp.Diagnostics.AddAttributeError(path.Root("ports"), "Unable to disable ports", "Unable to disable ports. "+err.Error())
			}
		} else {
			err := client.PortsSet(ctx, appName, portsToSet)
			if err != nil {
				resp.Diagnostics.AddAttributeError(path.Root("ports"), "Unable to set ports", "Unable to set ports. "+err.Error())
			}

			err = client.ProxyEnable(ctx, appName)
			if err != nil {
				resp.Diagnostics.AddAttributeError(path.Root("ports"), "Unable to enable ports", "Unable to enable ports. "+err.Error())
			}
		}
	}
	// --

	// -- domains
	needToSetDomains := false
	var domainsToSet []string

	// Get existing domains from state
	var existingDomains []string
	if !state.Domains.IsNull() && !state.Domains.IsUnknown() {
		stateElements := state.Domains.Elements()
		for _, elem := range stateElements {
			if stringVal, ok := elem.(basetypes.StringValue); ok {
				existingDomains = append(existingDomains, stringVal.ValueString())
			}
		}
	}

	// Get planned domains
	var planDomains []string
	if !plan.Domains.IsNull() && !plan.Domains.IsUnknown() {
		planElements := plan.Domains.Elements()
		for _, elem := range planElements {
			if stringVal, ok := elem.(basetypes.StringValue); ok {
				planDomains = append(planDomains, stringVal.ValueString())
			}
		}
	}

	// Check if domains need to be updated
	for _, existingDomain := range existingDomains {
		found := false
		for _, planDomain := range planDomains {
			if planDomain == existingDomain {
				found = true
				break
			}
		}
		if !found {
			needToSetDomains = true
		}
	}
	for _, planDomain := range planDomains {
		found := false
		for _, existingDomain := range existingDomains {
			if planDomain == existingDomain {
				found = true
				break
			}
		}
		if !found {
			needToSetDomains = true
		}
		domainsToSet = append(domainsToSet, planDomain)
	}
	if needToSetDomains {
		var err error
		if len(domainsToSet) == 0 {
			err = client.DomainsDisable(ctx, appName)
			if err != nil {
				resp.Diagnostics.AddAttributeError(path.Root("domains"), "Unable to disable domains support", "Unable to disable domains support. "+err.Error())
			}

			err = client.DomainsClear(ctx, appName)
			if err != nil {
				resp.Diagnostics.AddAttributeError(path.Root("domains"), "Unable to clear domains", "Unable to clear domains. "+err.Error())
			}
		} else {
			err = client.DomainsEnable(ctx, appName)
			if err != nil {
				resp.Diagnostics.AddAttributeError(path.Root("domains"), "Unable to enable domains support", "Unable to enable domains support. "+err.Error())
			}

			err = client.DomainsSet(ctx, appName, domainsToSet)
			if err != nil {
				resp.Diagnostics.AddAttributeError(path.Root("domains"), "Unable to set domains", "Unable to set domains. "+err.Error())
			}
		}
	}
	// TODO run letsencrypt:enable again after adding new domains
	// --

	// -- docker options
	var stateDockerOptionsElements map[string]attr.Value
	if !state.DockerOptions.IsNull() && !state.DockerOptions.IsUnknown() {
		stateDockerOptionsElements = state.DockerOptions.Elements()
	} else {
		stateDockerOptionsElements = make(map[string]attr.Value)
	}

	var planDockerOptionsElements map[string]attr.Value
	if !plan.DockerOptions.IsNull() && !plan.DockerOptions.IsUnknown() {
		planDockerOptionsElements = plan.DockerOptions.Elements()
	} else {
		planDockerOptionsElements = make(map[string]attr.Value)
	}

	for existingValue, existingDockerOptionValue := range stateDockerOptionsElements {
		found := false
		for planValue, planDockerOptionValue := range planDockerOptionsElements {
			if existingValue == planValue {
				found = true

				// Extract phase from existing docker option
				var existingPhase basetypes.SetValue
				if existingObj, ok := existingDockerOptionValue.(basetypes.ObjectValue); ok {
					attrs := existingObj.Attributes()
					if phaseAttr, exists := attrs["phase"]; exists {
						if phaseSet, ok := phaseAttr.(basetypes.SetValue); ok {
							existingPhase = phaseSet
						}
					}
				}

				// Extract phase from plan docker option
				var planPhase basetypes.SetValue
				if planObj, ok := planDockerOptionValue.(basetypes.ObjectValue); ok {
					attrs := planObj.Attributes()
					if phaseAttr, exists := attrs["phase"]; exists {
						if phaseSet, ok := phaseAttr.(basetypes.SetValue); ok {
							planPhase = phaseSet
						}
					}
				}

				if !existingPhase.Equal(planPhase) {
					err := client.DockerOptionRemove(ctx, appName, formatDockerOptionsPhases(existingPhase), existingValue)
					if err != nil {
						resp.Diagnostics.AddAttributeError(path.Root("docker_options").AtMapKey(existingValue), "Unable to remove docker option", "Unable to remove docker option. "+err.Error())
					}

					err = client.DockerOptionAdd(ctx, appName, formatDockerOptionsPhases(planPhase), planValue)
					if err != nil {
						resp.Diagnostics.AddAttributeError(path.Root("docker_options").AtMapKey(existingValue), "Unable to add docker option", "Unable to add docker option. "+err.Error())
					}

					restartRequired = true
				}

				break
			}
		}
		if !found {
			// Extract phase from existing docker option for removal
			if existingObj, ok := existingDockerOptionValue.(basetypes.ObjectValue); ok {
				attrs := existingObj.Attributes()
				if phaseAttr, exists := attrs["phase"]; exists {
					if phaseSet, ok := phaseAttr.(basetypes.SetValue); ok {
						err := client.DockerOptionRemove(ctx, appName, formatDockerOptionsPhases(phaseSet), existingValue)
						if err != nil {
							resp.Diagnostics.AddAttributeError(path.Root("docker_options").AtMapKey(existingValue), "Unable to remove docker option", "Unable to remove docker option. "+err.Error())
						}

						restartRequired = true
					}
				}
			}
		}
	}
	for planValue, planDockerOptionValue := range planDockerOptionsElements {
		found := false
		for existingValue := range stateDockerOptionsElements {
			if existingValue == planValue {
				found = true
				break
			}
		}
		if !found {
			// Extract phase from plan docker option for addition
			if planObj, ok := planDockerOptionValue.(basetypes.ObjectValue); ok {
				attrs := planObj.Attributes()
				if phaseAttr, exists := attrs["phase"]; exists {
					if phaseSet, ok := phaseAttr.(basetypes.SetValue); ok {
						err := client.DockerOptionAdd(ctx, appName, formatDockerOptionsPhases(phaseSet), planValue)
						if err != nil {
							resp.Diagnostics.AddAttributeError(path.Root("docker_options").AtMapKey(planValue), "Unable to add docker option", "Unable to add docker option. "+err.Error())
						}

						restartRequired = true
					}
				}
			}
		}
	}
	// --

	// -- networks
	if state.Networks != nil {
		if plan.Networks != nil {
			if !plan.Networks.AttachPostCreate.Equal(state.Networks.AttachPostCreate) {
				err := client.NetworkEnsureAndSetForApp(ctx, appName, "attach-post-create", plan.Networks.AttachPostCreate.ValueString())
				if err != nil {
					resp.Diagnostics.AddAttributeError(path.Root("networks").AtName("attach_post_create"), "Unable to set network", "Unable to set network. "+err.Error())
				}
			}
			if !plan.Networks.AttachPostDeploy.Equal(state.Networks.AttachPostDeploy) {
				err := client.NetworkEnsureAndSetForApp(ctx, appName, "attach-post-deploy", plan.Networks.AttachPostDeploy.ValueString())
				if err != nil {
					resp.Diagnostics.AddAttributeError(path.Root("networks").AtName("attach_post_deploy"), "Unable to set network", "Unable to set network. "+err.Error())
				}
			}
			if !plan.Networks.InitialNetwork.Equal(state.Networks.InitialNetwork) {
				err := client.NetworkEnsureAndSetForApp(ctx, appName, "initial-network", plan.Networks.InitialNetwork.ValueString())
				if err != nil {
					resp.Diagnostics.AddAttributeError(path.Root("networks").AtName("initial_network"), "Unable to set network", "Unable to set network. "+err.Error())
				}
			}
		} else {
			if !state.Networks.AttachPostCreate.IsNull() {
				err := client.NetworkUnsetForApp(ctx, appName, "attach-post-create")
				if err != nil {
					resp.Diagnostics.AddAttributeError(path.Root("networks").AtName("attach_post_create"), "Unable to unset network", "Unable to unset network. "+err.Error())
				}
			}
			if !state.Networks.AttachPostDeploy.IsNull() {
				err := client.NetworkUnsetForApp(ctx, appName, "attach-post-deploy")
				if err != nil {
					resp.Diagnostics.AddAttributeError(path.Root("networks").AtName("attach_post_deploy"), "Unable to unset network", "Unable to unset network. "+err.Error())
				}
			}
			if !state.Networks.InitialNetwork.IsNull() {
				err := client.NetworkUnsetForApp(ctx, appName, "initial-network")
				if err != nil {
					resp.Diagnostics.AddAttributeError(path.Root("networks").AtName("initial_network"), "Unable to unset network", "Unable to unset network. "+err.Error())
				}
			}
		}
	} else {
		if plan.Networks != nil {
			if !plan.Networks.AttachPostCreate.IsNull() {
				err := client.NetworkEnsureAndSetForApp(ctx, appName, "attach-post-create", plan.Networks.AttachPostCreate.ValueString())
				if err != nil {
					resp.Diagnostics.AddAttributeError(path.Root("networks").AtName("attach_post_create"), "Unable to set network", "Unable to set network. "+err.Error())
				}
			}
			if !plan.Networks.AttachPostDeploy.IsNull() {
				err := client.NetworkEnsureAndSetForApp(ctx, appName, "attach-post-deploy", plan.Networks.AttachPostDeploy.ValueString())
				if err != nil {
					resp.Diagnostics.AddAttributeError(path.Root("networks").AtName("attach_post_deploy"), "Unable to set network", "Unable to set network. "+err.Error())
				}
			}
			if !plan.Networks.InitialNetwork.IsNull() {
				err := client.NetworkEnsureAndSetForApp(ctx, appName, "initial-network", plan.Networks.InitialNetwork.ValueString())
				if err != nil {
					resp.Diagnostics.AddAttributeError(path.Root("networks").AtName("initial_network"), "Unable to set network", "Unable to set network. "+err.Error())
				}
			}
		}
	}
	// --

	// -- deploy
	if plan.Deploy != nil {
		deployed, err := r.deploy(ctx, client, plan.AppName.ValueString(), *plan.Deploy)
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("deploy"), "Unable to deploy", "Unable to deploy. "+err.Error())
		}
		if deployed {
			restartRequired = false
		}
	}
	// --

	if !resp.Diagnostics.HasError() && restartRequired {
		err := client.ProcessRestart(ctx, appName)
		if err != nil {
			resp.Diagnostics.AddError("Unable to restart process", "Unable to restart process. "+err.Error())
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}

	diags = resp.State.Set(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
}

// Delete deletes the resource and removes the Terraform state on success.
func (r *appResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	// Retrieve values from state
	var state appResourceModel
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
				"resource": "dokku_app",
				"app_name": state.AppName.ValueString(),
				"error":    err.Error(),
			})
			// Remove from state even though we couldn't connect
			return
		}
		resp.Diagnostics.AddError("SSH connection failed", err.Error())
		return
	}
	defer r.config.CloseClient(client)

	exists, err := client.AppExists(ctx, state.AppName.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("app_name"), "Unable to check app existence", "Unable to check app existence. "+err.Error())
		return
	}
	if !exists {
		return
	}

	// Delete existing app
	err = client.AppDestroy(ctx, state.AppName.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to destroy app", "Unable to destroy app. "+err.Error())
		return
	}
}

func (r *appResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Retrieve import ID and save to app_name attribute
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("app_name"), req.ID)...)
}

func (r *appResource) deploy(ctx context.Context, client *dokkuclient.Client, appName string, deployModel deployModel) (deployed bool, err error) {
	switch deployModel.Type.ValueString() {
	case "archive":
		err = client.DeployFromArchive(ctx, appName, deployModel.ArchiveType.ValueString(), deployModel.ArchiveUrl.ValueString())
		deployed = err == nil
	case "docker_image":
		if !deployModel.Login.IsNull() && !deployModel.Password.IsNull() {
			u, err := url.Parse("https://" + deployModel.DockerImage.ValueString())
			if err != nil {
				return false, fmt.Errorf("unable to parse url: %w", err)
			}
			err = client.RegistryLogin(ctx, u.Host, deployModel.Login.ValueString(), deployModel.Password.ValueString())
			if err != nil {
				return false, fmt.Errorf("unable to login to registry: %w", err)
			}
		}

		deployed, err = client.DeployFromImage(ctx, appName, deployModel.DockerImage.ValueString(), deployModel.AllowRebuild.ValueBool())
	case "git_repository":
		if !deployModel.Login.IsNull() && !deployModel.Password.IsNull() {
			u, err := url.Parse(deployModel.GitRepository.ValueString())
			if err != nil {
				return false, fmt.Errorf("unable to parse url: %w", err)
			}
			err = client.GitAuth(ctx, u.Host, deployModel.Login.ValueString(), deployModel.Password.ValueString())
			if err != nil {
				return false, fmt.Errorf("unable to login to git: %w", err)
			}
		}

		err = client.DeploySyncRepository(ctx, appName, deployModel.GitRepository.ValueString(), deployModel.GitRepositoryRef.ValueString())
		deployed = err == nil
	default:
		err = fmt.Errorf("Unknown deploy type %s", deployModel.Type.ValueString())
	}
	return
}

func formatDockerOptionsPhases(phasesSet types.Set) (phases []string) {
	for _, phase := range phasesSet.Elements() {
		//nolint:forcetypeassert
		phaseStr := phase.(types.String)
		phases = append(phases, phaseStr.ValueString())
	}
	return
}
