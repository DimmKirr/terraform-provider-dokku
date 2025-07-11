package config

import (
	"context"
	"fmt"
	"net"

	dokkuclient "github.com/aliksend/terraform-provider-dokku/provider/dokku_client"

	"github.com/blang/semver"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/melbahja/goph"
	"golang.org/x/crypto/ssh"
)

// DokkuConfig holds SSH configuration for lazy connection creation
type DokkuConfig struct {
	Host             string
	Port             uint
	User             string
	CertPath         string
	SkipHostKeyCheck bool
	HostKey          string
	LogSshCommands   bool
	UploadAppName    string
	UploadSplitBytes int
}

// NewClient creates a new dokkuClient with SSH connection on-demand
func (c *DokkuConfig) NewClient(ctx context.Context) (*dokkuclient.Client, error) {
	tflog.Debug(ctx, "Creating SSH connection", map[string]any{"host": c.Host, "port": c.Port, "user": c.User})

	// Get SSH authentication
	sshAuth, err := goph.Key(c.CertPath, "")
	if err != nil {
		return nil, fmt.Errorf("unable to find cert for SSH: %w", err)
	}

	// Configure SSH connection
	sshConfig := &goph.Config{
		Auth:     sshAuth,
		Addr:     c.Host,
		Port:     c.Port,
		User:     c.User,
		Callback: verifyHost,
	}

	if c.SkipHostKeyCheck {
		sshConfig.Callback = ssh.InsecureIgnoreHostKey()
	} else if c.HostKey != "" {
		_, _, publicKey, _, _, err := ssh.ParseKnownHosts([]byte(c.HostKey))
		if err != nil {
			return nil, fmt.Errorf("unable to parse provided ssh_host_key: %w", err)
		}
		sshConfig.Callback = ssh.FixedHostKey(publicKey)
	}

	// Establish SSH connection
	client, err := goph.NewConn(sshConfig)
	if err != nil {
		return nil, fmt.Errorf("unable to establish SSH connection: %w", err)
	}

	// Create dokku client
	dokkuClient := dokkuclient.New(client, c.LogSshCommands, c.UploadAppName, c.UploadSplitBytes)

	// Validate Dokku version on first connection
	rawVersion, version, err := dokkuClient.GetVersion(ctx)
	if err != nil {
		client.Close()
		if err == dokkuclient.ErrInvalidUser {
			return nil, err
		}
		return nil, fmt.Errorf("unable to get dokku version: %w", err)
	}

	testedVersions := ">=0.24.0 <= 0.34.7"
	if version.String() != "" {
		tflog.Debug(ctx, "host version", map[string]any{"version": version})

		compat := semver.MustParseRange(testedVersions)
		if !compat(version) {
			tflog.Warn(ctx, fmt.Sprintf("This provider has not been tested against Dokku version %s. Tested version range: %s", rawVersion, testedVersions))
		}
	}

	tflog.Debug(ctx, "SSH connection established successfully")
	return dokkuClient, nil
}

// CloseClient closes the underlying SSH connection
func (c *DokkuConfig) CloseClient(client *dokkuclient.Client) error {
	if client != nil && client.GetSSHClient() != nil {
		return client.GetSSHClient().Close()
	}
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
