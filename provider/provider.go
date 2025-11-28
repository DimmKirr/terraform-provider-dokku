package provider

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	configpkg "github.com/DimmKirr/terraform-provider-dokku/internal/config"
	"github.com/DimmKirr/terraform-provider-dokku/provider/services"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/melbahja/goph"
	"golang.org/x/crypto/ssh"
)

// Ensure dokkuProvider satisfies various provider interfaces.
var _ provider.Provider = &dokkuProvider{}

func New() provider.Provider {
	return &dokkuProvider{}
}

// dokkuProvider defines the provider implementation.
type dokkuProvider struct{}

// dokkuProviderModel describes the provider data model.
type dokkuProviderModel struct {
	SshHost                  types.String `tfsdk:"ssh_host"`
	SshPort                  types.Int64  `tfsdk:"ssh_port"`
	SshUser                  types.String `tfsdk:"ssh_user"`
	SshCert                  types.String `tfsdk:"ssh_cert"`
	SshSkipHostKeyCheck      types.Bool   `tfsdk:"ssh_skip_host_key_check"`
	SshHostKey               types.String `tfsdk:"ssh_host_key"`
	LogSshCommands           types.Bool   `tfsdk:"log_ssh_commands"`
	UploadAppName            types.String `tfsdk:"upload_app_name"`
	UploadSplitBytes         types.Int64  `tfsdk:"upload_split_bytes"`
	SkipUnreachableOnDestroy types.Bool   `tfsdk:"skip_unreachable_on_destroy"`
}

func (p *dokkuProvider) Metadata(ctx context.Context, req provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "dokku"
}

func (p *dokkuProvider) Schema(ctx context.Context, req provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Interact with dokku",
		Attributes: map[string]schema.Attribute{
			"ssh_host": schema.StringAttribute{
				Required:    true,
				Description: "Host to connect to",
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"ssh_port": schema.Int64Attribute{
				Optional:    true,
				Description: "Port to connect to. Default: 22",
			},
			"ssh_user": schema.StringAttribute{
				Optional:    true,
				Description: "Username to use. Default: dokku",
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"ssh_cert": schema.StringAttribute{
				Optional: true,
				Description: strings.Join([]string{
					"Certificate (private key) to use. Default: ~/.ssh/id_rsa",
					"",
					"Supported formats:",
					"- `file:/a` or `/a` or `./a` or `~/a` - use provided value as path to certificate file",
					"- `env:ABCD` or `$ABCD` - use env var ABCD",
					"- `raw:----...` or `----...` - use provided value as raw certificate",
				}, "\n  "),
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"ssh_skip_host_key_check": schema.BoolAttribute{
				Optional:    true,
				Description: "Skip the host key check. Insecure, should not be used in production. Default: false",
			},
			"ssh_host_key": schema.StringAttribute{
				Optional: true,
				Description: strings.Join([]string{
					"Host public key to use. By default key from ~/.ssh/known_hosts will be used.",
					"To get public keys for your ssh_host, run `ssh-keyscan <ssh_host>`.",
					"Must be set for usage within Terraform Cloud.",
				}, "\n  "),
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"log_ssh_commands": schema.BoolAttribute{
				Optional:    true,
				Description: "Print SSH commands with ERROR level",
			},
			"upload_app_name": schema.StringAttribute{
				Optional: true,
				Description: strings.Join([]string{
					"This attribute is used to upload local files to remote server using storage.local_directory attribute.",
					"App name to use for local file synchronization. Default: storage-sync",
					"",
					"Since dokku don't allow to upload files directly, workaround is used.",
					"Algorithm is:",
					"1. Create helper dokku application, using name provided in this attribute plus random string to prevent conflicts with simultaneous uploads",
					"2. Mount desired remote directory as /mnt",
					"3. Deploy \"busybox\" docker image to app",
					"4. [on host side] Create tar archive for local_directory and encode it using base64",
					"5. Connect to app using \"dokku enter\" and use a bunch of echo-s to make file \"tmp.tar.base64\"",
					"6. When file is completely uploaded - decode and un-tar it to /mnt",
					"7. Delete helper dokku application",
					"",
					"See details in description to \"upload_app_name\" attribute.",
				}, "\n  "),
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"upload_split_bytes": schema.Int64Attribute{
				Optional: true,
				Description: strings.Join([]string{
					"This attribute is used to upload local files to remote server using storage.local_directory attribute.",
					"Number of bytes to split uploaded base64-encoded tar archive. See details in description to \"upload_app_name\" attribute. Default: 256",
					"",
					"Due to limited length of commands we can't use one echo to copy entire file.",
					"So we need to split file into parts no larger than upload_split_bytes.",
					"Don't use big values because if length of command exceed the limit all operation will hang out.",
				}, "\n  "),
				Validators: []validator.Int64{
					int64validator.AtLeast(1),
				},
			},
			"skip_unreachable_on_destroy": schema.BoolAttribute{
				Optional: true,
				Description: strings.Join([]string{
					"If true, skip SSH connection failures during destroy operations and remove resources from state anyway.",
					"Useful when infrastructure has already been destroyed outside of Terraform (e.g., VM deleted, network unreachable).",
					"Only affects destroy operations - create/update/read will still fail on connection errors.",
					"Default: false",
				}, "\n  "),
			},
		},
	}
}

func (p *dokkuProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	// Retrieve provider data from configuration
	var config dokkuProviderModel
	diags := req.Config.Get(ctx, &config)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Allow unknown values during configuration - validation will happen
	// when resources actually try to establish connections (lazy connection)

	// Default values to environment variables, but override
	// with Terraform configuration value if set.

	host := ""
	port := uint(22)
	sshUsername := "dokku"
	sshCertPath := "~/.ssh/id_rsa"
	logSshCommands := false
	uploadAppName := "storage-sync"
	uploadSplitBytes := 256
	skipUnreachableOnDestroy := false

	if config.SshHost.IsUnknown() {
		tflog.Debug(ctx, "SSH host is unknown (will be known after apply) - deferring validation to resource operations")
	} else if !config.SshHost.IsNull() {
		host = config.SshHost.ValueString()
	}
	if config.SshPort.IsUnknown() {
		tflog.Debug(ctx, "SSH port is unknown (will be known after apply)")
	} else if !config.SshPort.IsNull() {
		port = uint(config.SshPort.ValueInt64())
	}

	if config.SshUser.IsUnknown() {
		tflog.Debug(ctx, "SSH user is unknown (will be known after apply)")
	} else if !config.SshUser.IsNull() {
		sshUsername = config.SshUser.ValueString()
	}

	if config.SshCert.IsUnknown() {
		tflog.Debug(ctx, "SSH cert is unknown (will be known after apply)")
	} else if !config.SshCert.IsNull() {
		var err error
		sshCertPath, err = getCertFilename(ctx, config.SshCert.ValueString())
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("ssh_cert"), "Unable to read cert", "Unable to read cert. "+err.Error())
			return
		}
	}
	if !config.LogSshCommands.IsNull() {
		logSshCommands = config.LogSshCommands.ValueBool()
	}
	if !config.UploadAppName.IsNull() {
		uploadAppName = config.UploadAppName.ValueString()
	}
	if !config.UploadSplitBytes.IsNull() {
		uploadSplitBytes = int(config.UploadSplitBytes.ValueInt64())
	}
	if !config.SkipUnreachableOnDestroy.IsNull() {
		skipUnreachableOnDestroy = config.SkipUnreachableOnDestroy.ValueBool()
	}

	usr, err := user.Current()
	if err == nil {
		_ = os.MkdirAll(filepath.Join(usr.HomeDir, ".ssh"), os.ModePerm)
	}

	sshCertPath, err = resolveHomeDir(sshCertPath)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("ssh_cert"), "Unable to get SSH cert", "Unable to get SSH cert. "+err.Error())
		return
	}

	// Don't validate empty/missing values here - they may be computed (unknown during plan)
	// Validation will happen when resources try to establish connections via NewClient()

	if host == "" {
		tflog.Debug(ctx, "Provider configured with empty/unknown SSH connection details - will be validated when resources attempt to connect")
	} else {
		tflog.Debug(ctx, "Provider configured for SSH connections", map[string]any{"host": host, "port": port, "user": sshUsername})
	}

	// Parse host key configuration
	skipHostKeyCheck := false
	if !config.SshSkipHostKeyCheck.IsNull() {
		skipHostKeyCheck = config.SshSkipHostKeyCheck.ValueBool()
	}

	hostKey := ""
	if !config.SshHostKey.IsNull() {
		hostKey = config.SshHostKey.ValueString()
	}

	// Create DokkuConfig for lazy connection establishment
	dokkuConfig := &configpkg.DokkuConfig{
		Host:                     host,
		Port:                     port,
		User:                     sshUsername,
		CertPath:                 sshCertPath,
		SkipHostKeyCheck:         skipHostKeyCheck,
		HostKey:                  hostKey,
		LogSshCommands:           logSshCommands,
		UploadAppName:            uploadAppName,
		UploadSplitBytes:         uploadSplitBytes,
		SkipUnreachableOnDestroy: skipUnreachableOnDestroy,
	}

	tflog.Debug(ctx, "Provider configured for lazy SSH connections")

	// Make the dokku config available during DataSource and Resource
	// type Configure methods - connections will be established on-demand
	resp.DataSourceData = dokkuConfig
	resp.ResourceData = dokkuConfig
}

func (p *dokkuProvider) Resources(ctx context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewAppResource,
		NewDomainResource,
		NewHttpAuthResource,
		NewLetsencryptResource,
		NewPluginResource,
		NewNginxConfigResource,

		services.NewClickhouseLinkResource,
		services.NewClickhouseResource,
		services.NewCouchDBLinkResource,
		services.NewCouchDBResource,
		services.NewElasticsearchLinkResource,
		services.NewElasticsearchResource,
		services.NewMariaDBLinkResource,
		services.NewMariaDBResource,
		services.NewMongoLinkResource,
		services.NewMongoResource,
		services.NewMysqlLinkResource,
		services.NewMysqlResource,
		services.NewNatsLinkResource,
		services.NewNatsResource,
		services.NewPostgresLinkResource,
		services.NewPostgresResource,
		services.NewRabbitMQLinkResource,
		services.NewRabbitMQResource,
		services.NewRedisLinkResource,
		services.NewRedisResource,
		services.NewRethinkDBLinkResource,
		services.NewRethinkDBResource,
	}
}

func (p *dokkuProvider) DataSources(ctx context.Context) []func() datasource.DataSource {
	return nil
}

func verifyHost(host string, remote net.Addr, key ssh.PublicKey) error {
	// If you want to connect to new hosts.
	// here your should check new connections public keys
	// if the key not trusted you shuld return an error

	// hostFound: is host in known hosts file.
	// err: error if key not in known hosts file OR host in known hosts file but key changed!
	hostFound, err := goph.CheckKnownHost(host, remote, key, "")

	// Host in known hosts but key mismatch!
	// Maybe because of MAN IN THE MIDDLE ATTACK!
	if hostFound && err != nil {

		return err
	}

	// handshake because public key already exists.
	if hostFound && err == nil {

		return nil
	}

	// // Ask user to check if he trust the host public key.
	// if askIsHostTrusted(host, key) == false {

	// 	// Make sure to return error on non trusted keys.
	// 	return errors.New("you typed no, aborted!")
	// }

	// Add the new host to known hosts file.
	return goph.AddKnownHost(host, remote, key, "")
}

func tmpFileWithValue(value string) (string, error) {
	file, err := ioutil.TempFile("", "ssh_cert")
	if err != nil {
		return "", err
	}
	err = ioutil.WriteFile(file.Name(), []byte(value), os.ModePerm)
	if err != nil {
		return "", err
	}
	return file.Name(), nil
}

func getCertFilename(ctx context.Context, value string) (certPath string, err error) {
	if value == "" {
		return "", fmt.Errorf("Value for cert must be provided")
	}

	parts := strings.Split(value, ":")
	switch parts[0] {
	case "file":
		certPath = parts[1]
	case "env":
		certPath, err = tmpFileWithValue(os.Getenv(parts[1]))
		if err != nil {
			return "", fmt.Errorf("Unable to create temp file: %w", err)
		}
		tflog.Debug(ctx, "Save ssh_cert from env var to tmp file", map[string]any{"certPath": certPath})
	case "raw":
		var err error
		certPath, err = tmpFileWithValue(parts[1])
		if err != nil {
			return "", fmt.Errorf("Unable to create temp file: %w", err)
		}
		tflog.Debug(ctx, "Save ssh_cert from raw string to tmp file", map[string]any{"certPath": certPath})
	default:
		if value[0] == '~' || value[1] == '/' || value[1] == '.' {
			certPath = value
		} else if value[0] == '$' {
			var err error
			certPath, err = tmpFileWithValue(os.Getenv(value[1:]))
			if err != nil {
				return "", fmt.Errorf("Unable to create temp file: %w", err)
			}
			tflog.Debug(ctx, "Save ssh_cert from env var to tmp file", map[string]any{"certPath": certPath})
		} else if value[0] == '-' {
			var err error
			certPath, err = tmpFileWithValue(value)
			if err != nil {
				return "", fmt.Errorf("Unable to create temp file: %w", err)
			}
			tflog.Debug(ctx, "Save ssh_cert from raw string to tmp file", map[string]any{"certPath": certPath})
		} else {
			return "", fmt.Errorf("Unknown cert format")
		}
	}

	return
}

func resolveHomeDir(path string) (string, error) {
	if !strings.HasPrefix(path, "~/") {
		return path, nil
	}

	usr, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("Unable to get current user: %w", err)
	}
	return filepath.Join(usr.HomeDir, path[2:]), nil
}
