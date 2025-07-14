package test

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gruntwork-io/terratest/modules/docker"
	"github.com/gruntwork-io/terratest/modules/ssh"
	"github.com/gruntwork-io/terratest/modules/terraform"
	"github.com/gruntwork-io/terratest/modules/test-structure"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	cryptossh "golang.org/x/crypto/ssh"
)

// Shared infrastructure for all E2E tests

// getDokkuVersion returns the Dokku version to use, either from DOKKU_VERSION env var or default
func getDokkuVersion() string {
	version := os.Getenv("DOKKU_VERSION")
	if version == "" {
		version = "0.34.7" // Default version if not specified
	}
	return version
}

// testEnvironment holds all resources for a single test
type testEnvironment struct {
	ContainerName string
	NetworkName   string
	SSHKeys       *testSSHKeys
	InternalPorts map[string]string
	ExternalPorts map[string]string
	TestDir       string
}

type testSSHKeys struct {
	privateKeyPEM  string
	publicKeySSH   string
	privateKeyPath string
	publicKeyPath  string
}

// createTestEnvironment sets up an isolated test environment for parallel execution
func createTestEnvironment(t *testing.T, testDir string) *testEnvironment {
	// Generate unique names for resources using test name and timestamp to avoid conflicts
	timestamp := time.Now().UnixNano()
	testName := strings.ReplaceAll(t.Name(), "/", "-")

	containerName := fmt.Sprintf("dokku-test-%s-%d", testName, timestamp)
	networkName := fmt.Sprintf("network-%s-%d", testName, timestamp)

	// Check if we're in a CI environment (GitHub Actions)
	isCI := os.Getenv("CI") == "true" || os.Getenv("GITHUB_ACTIONS") == "true"

	if isCI {
		// In CI environments, skip custom network creation to avoid network isolation issues
		// between Dokku container and app containers that Dokku creates
		t.Logf("CI environment detected, skipping custom network creation to prevent isolation issues")
		networkName = ""
	} else {
		// Create Docker network for test isolation in local development
		createNetworkCmd := exec.Command("docker", "network", "create", networkName)
		if err := createNetworkCmd.Run(); err != nil {
			t.Logf("Warning: Failed to create network %s: %v", networkName, err)
			// Continue without custom network - tests will use default bridge
			networkName = ""
		} else {
			t.Logf("Created Docker network: %s", networkName)
		}
	}

	// Generate dynamic external ports to avoid conflicts
	sshPort := findAvailablePort(t, "ssh")
	httpPort := findAvailablePort(t, "http")

	// Generate SSH keys for this test environment
	sshKeys := generateSSHKeys(t, testDir)

	return &testEnvironment{
		ContainerName: containerName,
		NetworkName:   networkName,
		SSHKeys:       sshKeys,
		InternalPorts: map[string]string{
			"ssh":  "22",
			"http": "80",
		},
		ExternalPorts: map[string]string{
			"ssh":  strconv.Itoa(sshPort),
			"http": strconv.Itoa(httpPort),
		},
		TestDir: testDir,
	}
}

// findAvailablePort finds and returns an available TCP port based on test name for consistency across CI/CD.
// Uses MD5 hash of test name to generate a deterministic starting port below 40000.
// If the calculated port is occupied, it tries port+1, port+2, etc. until finding an available one.
// isPortAvailable checks if a port is available by testing both network binding and Docker container usage
func isPortAvailable(t *testing.T, port int) bool {
	// First check if we can bind to the port
	listener, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", port))
	if err != nil {
		return false // Port is already in use by some process
	}
	listener.Close()

	// Also check if Docker is using this port
	cmd := exec.Command("docker", "ps", "--format", "{{.Ports}}")
	output, err := cmd.Output()
	if err != nil {
		// If docker command fails, just rely on the network check
		return true
	}

	// Check if any container is using this port
	portStr := fmt.Sprintf(":%d->", port)
	if strings.Contains(string(output), portStr) {
		return false // Docker container is using this port
	}

	return true // Port is available
}

func findAvailablePort(t *testing.T, portType string) int {
	t.Helper()

	// Calculate deterministic port based on test name and port type
	testName := t.Name()
	portKey := fmt.Sprintf("%s-%s", testName, portType)
	hash := md5.Sum([]byte(portKey))

	// Convert first 4 bytes of hash to uint32, then mod to get port in range 30000-39999
	hashValue := binary.BigEndian.Uint32(hash[:4])
	basePort := int(hashValue%10000) + 30000

	// Ensure port is below 40000
	if basePort >= 40000 {
		basePort = basePort - 10000
	}

	// Try to find an available port starting from the calculated base port
	for port := basePort; port < 40000; port++ {
		if isPortAvailable(t, port) {
			t.Logf("Found deterministic available port: %d (base: %d, test: %s, type: %s)", port, basePort, testName, portType)
			return port
		}
		// Port is occupied, try next one
	}

	// Fallback to random port if all deterministic ports are occupied
	t.Logf("Falling back to a random port")
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("Failed to find any available port: %v", err)
	}
	defer listener.Close()

	addr := listener.Addr().(*net.TCPAddr)
	port := addr.Port
	t.Logf("Fallback to random available port: %d (deterministic range 30000-39999 was full, test: %s, type: %s)", port, testName, portType)
	return port
}

// cleanupTestEnvironment cleans up all resources for a test environment
func cleanupTestEnvironment(t *testing.T, env *testEnvironment) {
	if env == nil {
		return
	}

	// Stop and remove container
	if env.ContainerName != "" {
		stopCmd := exec.Command("docker", "stop", env.ContainerName)
		stopCmd.Run() // Ignore errors - container might already be stopped

		removeCmd := exec.Command("docker", "rm", "-f", env.ContainerName)
		removeCmd.Run() // Ignore errors - container might not exist

		t.Logf("Cleaned up container: %s", env.ContainerName)
	}

	// Remove network if we created one
	if env.NetworkName != "" {
		removeNetworkCmd := exec.Command("docker", "network", "rm", env.NetworkName)
		if err := removeNetworkCmd.Run(); err != nil {
			t.Logf("Warning: Failed to remove network %s: %v", env.NetworkName, err)
		} else {
			t.Logf("Cleaned up network: %s", env.NetworkName)
		}
	}

	// Clean up test files
	cleanupTestFiles(t, env.TestDir)
}

func generateSSHKeys(t *testing.T, testDir string) *testSSHKeys {
	// Generate RSA private key
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err, "Failed to generate private key")

	// Encode private key to PEM format
	privateKeyPEM := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	}
	privateKeyBytes := pem.EncodeToMemory(privateKeyPEM)

	// Generate public key in SSH format
	publicKey, err := cryptossh.NewPublicKey(&privateKey.PublicKey)
	require.NoError(t, err, "Failed to generate public key")
	publicKeySSH := string(cryptossh.MarshalAuthorizedKey(publicKey))

	// Ensure the directory exists
	err = os.MkdirAll(testDir, 0755)
	require.NoError(t, err, "Failed to create test directory")

	// Write keys to files
	privateKeyPath := filepath.Join(testDir, "ssh_key")
	publicKeyPath := filepath.Join(testDir, "ssh_key.pub")

	err = os.WriteFile(privateKeyPath, privateKeyBytes, 0600)
	require.NoError(t, err, "Failed to write private key")

	err = os.WriteFile(publicKeyPath, []byte(publicKeySSH), 0644)
	require.NoError(t, err, "Failed to write public key")

	t.Logf("Generated SSH keys: %s, %s", privateKeyPath, publicKeyPath)

	return &testSSHKeys{
		privateKeyPEM:  string(privateKeyBytes),
		publicKeySSH:   strings.TrimSpace(publicKeySSH),
		privateKeyPath: privateKeyPath,
		publicKeyPath:  publicKeyPath,
	}
}

func setupDokkuContainer(t *testing.T, env *testEnvironment) {
	// Try to remove any existing container with the same name
	cleanupCmd := exec.Command("docker", "rm", "-f", env.ContainerName)
	cleanupCmd.Run()

	// Get the Dokku version to use
	dokkuVersion := getDokkuVersion()
	dokkuImageName := fmt.Sprintf("dokku/dokku:%s", dokkuVersion)

	// Prepare run options with dynamic naming and networking
	otherOptions := []string{
		"-e", "DOKKU_HOSTNAME=dokku.test",
		"-e", "DOKKU_HOST_ROOT=/home/dokku/dokku",
		"-v", "/var/run/docker.sock:/var/run/docker.sock",
		"--add-host", "host.docker.internal:host-gateway",
	}

	// Always use port mapping for external access, regardless of network
	otherOptions = append(otherOptions,
		"-p", fmt.Sprintf("%s:%s", env.ExternalPorts["ssh"], env.InternalPorts["ssh"]),
		"-p", fmt.Sprintf("%s:%s", env.ExternalPorts["http"], env.InternalPorts["http"]),
	)

	// Add network configuration if we have a custom network (for container-to-container communication)
	if env.NetworkName != "" {
		otherOptions = append(otherOptions, "--network", env.NetworkName)
	}

	// Run the Dokku container
	runOptions := &docker.RunOptions{
		Name:         env.ContainerName,
		Detach:       true,
		Privileged:   true,
		OtherOptions: otherOptions,
	}

	docker.Run(t, dokkuImageName, runOptions)

	// Wait for container to be up and running
	retries := 30
	retryInterval := 3 * time.Second
	sshServiceRunning := false

	for i := 0; i < retries; i++ {
		sshCheckCmd := exec.Command("docker", "exec", env.ContainerName, "service", "ssh", "status")
		err := sshCheckCmd.Run()
		if err == nil {
			sshServiceRunning = true
			break
		}
		t.Logf("SSH service check attempt %d failed: %v", i+1, err)
		time.Sleep(retryInterval)
	}

	// Ensure dokku is installed and working
	dokkuInstalled := false
	if sshServiceRunning {
		// Check if dokku command works
		dokkuCheckCmd := exec.Command("docker", "exec", env.ContainerName, "dokku", "--version")
		err := dokkuCheckCmd.Run()
		if err == nil {
			dokkuInstalled = true
		}
	}

	// Only proceed if both SSH and Dokku are working
	require.True(t, sshServiceRunning && dokkuInstalled, "Container basic checks passed")

	// Verify container is actually running and ports are mapped
	inspectCmd := exec.Command("docker", "inspect", env.ContainerName, "--format", "{{.State.Running}} {{.NetworkSettings.Ports}}")
	inspectOutput, err := inspectCmd.CombinedOutput()
	if err != nil {
		t.Logf("Container inspection failed: %v", err)
	} else {
		t.Logf("Container status and ports: %s", string(inspectOutput))
	}

	// Wait additional time to ensure services are fully stabilized
	t.Logf("Container ready, waiting additional 15 seconds for services to fully stabilize...")
	time.Sleep(15 * time.Second)
}

func setupSSH(t *testing.T, env *testEnvironment) {
	// Use the generated SSH keys
	pubKey := env.SSHKeys.publicKeySSH
	privateKeyPath := env.SSHKeys.privateKeyPath

	// Fix permissions on private key first
	exec.Command("chmod", "600", privateKeyPath).Run()

	// Use a correct forced command format that works with dokku
	authorizedKeyEntry := fmt.Sprintf(`command="dokku $SSH_ORIGINAL_COMMAND",no-port-forwarding,no-agent-forwarding %s dokku-test-key`, pubKey)

	// Multiple attempts to set up SSH properly
	for attempt := 0; attempt < 3; attempt++ {
		// Configure SSH key in the container with more thorough setup
		setupCmd := exec.Command("docker", "exec", env.ContainerName, "bash", "-c", fmt.Sprintf(`
			# Ensure dokku user exists
			id dokku || exit 1
			
			# Create .ssh directory with proper permissions
			mkdir -p /home/dokku/.ssh
			chmod 700 /home/dokku/.ssh
			
			# Add the SSH key
			echo '%s' > /home/dokku/.ssh/authorized_keys
			chmod 600 /home/dokku/.ssh/authorized_keys
			chown -R dokku:dokku /home/dokku/.ssh
			
			# Allow passwordless sudo for dokku user
			echo 'dokku ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/dokku
			chmod 0440 /etc/sudoers.d/dokku

			# Ensure SSH daemon is running
			service ssh start || service sshd start
			sleep 2
			
			# Test that SSH service is listening
			netstat -tlnp | grep :22 || ss -tlnp | grep :22
		`, authorizedKeyEntry))

		output, err := setupCmd.CombinedOutput()
		t.Logf("SSH setup attempt %d output: %s", attempt+1, string(output))

		if err == nil {
			break
		}

		if attempt == 2 {
			t.Logf("SSH setup failed after 3 attempts: %v", err)
		}
		time.Sleep(2 * time.Second)
	}

	// Give SSH service more time to fully start
	time.Sleep(10 * time.Second)

	// Test port connectivity
	t.Logf("Testing port connectivity to localhost:%s", env.ExternalPorts["ssh"])
	portCmd := exec.Command("nc", "-zv", "localhost", env.ExternalPorts["ssh"])
	for i := 0; i < 5; i++ {
		output, err := portCmd.CombinedOutput()
		t.Logf("Port test attempt %d: %v, output: %s", i+1, err, string(output))
		if err == nil {
			t.Logf("Port %s is accessible", env.ExternalPorts["ssh"])
			break
		}
		time.Sleep(2 * time.Second)
	}

	// Verify SSH connectivity with the dokku version command
	maxRetries := 5
	retryDelay := 3 * time.Second

	for i := 0; i < maxRetries; i++ {
		sshCmd := exec.Command("ssh", "-F", "/dev/null", // Bypass user SSH config
			"-i", privateKeyPath,
			"-p", env.ExternalPorts["ssh"],
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "ConnectTimeout=10",
			"-o", "LogLevel=ERROR",
			"dokku@localhost",
			"version")

		output, err := sshCmd.CombinedOutput()
		t.Logf("SSH test attempt %d: %v, output: %s", i+1, err, string(output))

		if err == nil {
			t.Logf("SSH connection successful!")
			return
		}

		if i < maxRetries-1 {
			t.Logf("SSH connection attempt %d failed, retrying in %v", i+1, retryDelay)
			time.Sleep(retryDelay)
		}
	}

	t.Logf("Warning: SSH connection test failed after %d attempts, but continuing anyway", maxRetries)
}

func isContainerReady(t *testing.T, env *testEnvironment) bool {
	// Check if we can SSH to the container and run a dokku command
	keys := env.SSHKeys
	keyFile := keys.privateKeyPath
	if _, err := os.Stat(keyFile); os.IsNotExist(err) {
		t.Logf("SSH key file not found at %s", keyFile)
		return false
	}

	// Fix key permissions if needed
	exec.Command("chmod", "600", keyFile).Run()

	// Get the Dokku version to use
	dokkuVersion := getDokkuVersion()

	// Determine connection details
	sshHost := "localhost"
	sshPort := env.ExternalPorts["ssh"]

	// Try SSH connection with the key and test dokku command
	cmd := exec.Command("ssh", "-F", "/dev/null",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-i", keyFile,
		"-p", sshPort,
		fmt.Sprintf("dokku@%s", sshHost),
		"--quiet version")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("SSH connection failed: %v - %s", err, string(output))
		return false
	}

	// Verify we get the expected version response
	outputStr := string(output)
	if !strings.Contains(outputStr, dokkuVersion) && !strings.Contains(outputStr, "version") {
		t.Logf("Unexpected dokku version output: %s", outputStr)
		return false
	}

	t.Logf("Container fully ready with SSH access!")
	return true
}

func getLocalSSHPrivateKey(t *testing.T, keys *testSSHKeys) string {
	return strings.TrimSpace(keys.privateKeyPEM)
}

func getLocalSSHPublicKey(t *testing.T, keys *testSSHKeys) string {
	return keys.publicKeySSH
}

func generateProviderConfig(t *testing.T, testDir string) {
	providerTF := `terraform {
  required_providers {
    dokku = {
      source = "localhost/providers/dokku"
    }
  }
}

provider "dokku" {
  ssh_host                = var.dokku_host
  ssh_port                = var.dokku_port
  ssh_user                = "dokku"
  ssh_cert                = var.ssh_private_key
  ssh_skip_host_key_check = true
  log_ssh_commands        = true
}
`

	providerFile := filepath.Join(testDir, "provider.tf")
	err := os.WriteFile(providerFile, []byte(providerTF), 0644)
	require.NoError(t, err, "Failed to write provider.tf")
}

func generateVariables(t *testing.T, testDir string) {
	variablesTF := `variable "dokku_host" {
  description = "Dokku server hostname or IP"
  type        = string
  default     = "localhost"
}

variable "dokku_port" {
  description = "SSH port for Dokku server"
  type        = number
  default     = 3022
}

variable "ssh_private_key" {
  description = "SSH private key content"
  type        = string
  sensitive   = true
}

variable "docker_image" {
  description = "Docker image to deploy"
  type        = string
  default     = "jmalloc/echo-server"
}
`

	variablesFile := filepath.Join(testDir, "variables.tf")
	err := os.WriteFile(variablesFile, []byte(variablesTF), 0644)
	require.NoError(t, err, "Failed to write variables.tf")
}

func generateTerraformRC(t *testing.T, testDir string) {
	tfrcContent := `provider_installation {
  dev_overrides {
    "localhost/providers/dokku" = "` + testDir + `"
  }
  direct {}
}
`

	tfrcFile := filepath.Join(testDir, ".terraformrc")
	err := os.WriteFile(tfrcFile, []byte(tfrcContent), 0644)
	require.NoError(t, err, "Failed to write .terraformrc")
}

func buildProvider(t *testing.T, testDir string) {
	providerBinary := filepath.Join(testDir, "terraform-provider-dokku")

	// Check if provider binary already exists (e.g., from CI artifact)
	if _, err := os.Stat(providerBinary); os.IsNotExist(err) {
		// Find the actual project root by looking for go.mod
		projectRoot := findProjectRoot(t)

		// Check if there's a pre-built binary in the project root (CI case)
		projectBinary := filepath.Join(projectRoot, "terraform-provider-dokku")
		if _, err := os.Stat(projectBinary); err == nil {
			// Copy the pre-built binary
			input, err := os.ReadFile(projectBinary)
			require.NoError(t, err, "Failed to read pre-built provider")

			err = os.WriteFile(providerBinary, input, 0755)
			require.NoError(t, err, "Failed to copy pre-built provider")

			t.Logf("Copied pre-built provider from: %s to: %s", projectBinary, providerBinary)
		} else {
			// Build from source (local development case)
			buildArgs := []string{"build", "-o", providerBinary, "."}

			cmd := exec.Command("go", buildArgs...)
			cmd.Dir = projectRoot

			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "Failed to build provider: %s", string(output))

			t.Logf("Built provider from source at: %s", providerBinary)
		}
	} else {
		t.Logf("Provider binary already exists at: %s", providerBinary)
	}

	// Set TF_CLI_CONFIG_FILE to point to our dev override config
	rcPath := filepath.Join(testDir, ".terraformrc")
	os.Setenv("TF_CLI_CONFIG_FILE", rcPath)

	t.Logf("Terraform RC: %s", rcPath)
}

func findProjectRoot(t *testing.T) string {
	// Find the actual project root by looking for go.mod
	wd, err := os.Getwd()
	require.NoError(t, err)

	projectRoot := wd
	for {
		if _, err := os.Stat(filepath.Join(projectRoot, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(projectRoot)
		if parent == projectRoot {
			t.Fatal("Could not find project root (go.mod not found)")
		}
		projectRoot = parent
	}

	t.Logf("Found project root: %s", projectRoot)
	return projectRoot
}

func logAppStatus(t *testing.T, host ssh.Host, appName string, output string, reportOutput string) {
	// Test 3: Verify deployment status
	deployOutput, err := ssh.CheckSshCommandE(t, host, fmt.Sprintf("ps:report %s", appName))
	if err != nil {
		t.Logf("Deploy status check failed (this may be normal): %v", err)
	} else {
		t.Logf("Deploy status: %s", deployOutput)
	}

	// Test 4: Verify port mapping/proxy configuration
	proxyOutput, err := ssh.CheckSshCommandE(t, host, fmt.Sprintf("proxy:ports %s", appName))
	if err != nil {
		t.Logf("Proxy ports check failed (this may be normal): %v", err)
	} else {
		t.Logf("Proxy ports: %s", proxyOutput)
	}

	t.Logf("App list: %s", output)
	t.Logf("Report: %s", reportOutput)
}

func validateSimpleApp(t *testing.T, host ssh.Host, appName string) {
	// Verify the app exists
	t.Logf("Validating simple app: %s", appName)

	// Check if the app exists
	appListOutput, err := ssh.CheckSshCommandE(t, host, "apps:list")
	require.NoError(t, err, "Failed to list apps")
	assert.Contains(t, appListOutput, appName, "App should be in the apps list")

	// Verify the app is deployed with jmalloc/echo-server
	imageInfoOutput, err := ssh.CheckSshCommandE(t, host, fmt.Sprintf("ps:inspect %s", appName))
	require.NoError(t, err, "Failed to inspect app")
	assert.Contains(t, imageInfoOutput, "jmalloc/echo-server", "App should be deployed with jmalloc/echo-server image")

	// Check if app is running
	psOutput, err := ssh.CheckSshCommandE(t, host, fmt.Sprintf("ps:report %s", appName))
	require.NoError(t, err, "Failed to get process status")
	assert.Contains(t, psOutput, "Running", "App should be running")

	// Check if the app has the default domain
	domainsOutput, err := ssh.CheckSshCommandE(t, host, fmt.Sprintf("domains:report %s", appName))
	require.NoError(t, err, "Failed to get domains report")
	assert.Contains(t, domainsOutput, fmt.Sprintf("%s.dokku.test", appName), "Default domain should be configured")

	// Log the validation results
	t.Logf("App status: %s", psOutput)
	t.Logf("App domains: %s", domainsOutput)
	t.Logf("App image info: %s", imageInfoOutput)
}

func validateComplexApp(t *testing.T, host ssh.Host, appName string) {
	// Verify complex config vars including ENV='prod' and APP_NAME
	configOutput, err := ssh.CheckSshCommandE(t, host, fmt.Sprintf("config %s", appName))
	require.NoError(t, err, "Failed to get config")
	assert.Contains(t, configOutput, "ENV", "ENV should be set")
	assert.Contains(t, configOutput, "prod", "ENV should be set to prod")
	assert.Contains(t, configOutput, "APP_NAME", "APP_NAME should be set")
	assert.Contains(t, configOutput, appName, "APP_NAME should match app name")
	assert.Contains(t, configOutput, "NODE_ENV", "NODE_ENV should be set")
	assert.Contains(t, configOutput, "production", "NODE_ENV should be set to production")
	assert.Contains(t, configOutput, "API_URL", "API_URL should be set")
	assert.Contains(t, configOutput, "https://api.example.com", "API_URL should be set correctly")

	// Verify multiple domains
	domainsOutput, err := ssh.CheckSshCommandE(t, host, fmt.Sprintf("domains:report %s", appName))
	require.NoError(t, err, "Failed to get domains report")
	assert.Contains(t, domainsOutput, fmt.Sprintf("%s.dokku.test", appName), "Primary domain should be configured")
	assert.Contains(t, domainsOutput, fmt.Sprintf("api.%s.dokku.test", appName), "API domain should be configured")

	t.Logf("Complex app config: %s", configOutput)
	t.Logf("Complex app domains: %s", domainsOutput)
}

func validateDefaultApp(t *testing.T, host ssh.Host, appName string) {
	// Default validation for backward compatibility
	configOutput, err := ssh.CheckSshCommandE(t, host, fmt.Sprintf("config %s", appName))
	require.NoError(t, err, "Failed to get config")
	assert.Contains(t, configOutput, "NODE_ENV", "NODE_ENV should be set")
	assert.Contains(t, configOutput, "production", "NODE_ENV should be set to production")
	assert.Contains(t, configOutput, "PORT", "PORT should be set")

	// Verify domains
	domainsOutput, err := ssh.CheckSshCommandE(t, host, fmt.Sprintf("domains:report %s", appName))
	require.NoError(t, err, "Failed to get domains report")
	assert.Contains(t, domainsOutput, fmt.Sprintf("%s.dokku.test", appName), "Domain should be configured")

	t.Logf("Config: %s", configOutput)
	t.Logf("Domains: %s", domainsOutput)
}

func validateDokkuAppExample(t *testing.T, host ssh.Host, appName string) {
	// Verify the app exists
	t.Logf("Validating dokku_app example: %s", appName)

	// Check if the app exists
	appListOutput, err := ssh.CheckSshCommandE(t, host, "apps:list")
	require.NoError(t, err, "Failed to list apps")
	assert.Contains(t, appListOutput, appName, "App should be in the apps list")

	// Verify the app is deployed with jmalloc/echo-server image
	imageInfoOutput, err := ssh.CheckSshCommandE(t, host, fmt.Sprintf("ps:inspect %s", appName))
	require.NoError(t, err, "Failed to inspect app")
	assert.Contains(t, imageInfoOutput, "jmalloc/echo-server", "App should be deployed with jmalloc/echo-server image")

	// Check if app has expected configuration (minimal config for demo app)
	configOutput, err := ssh.CheckSshCommandE(t, host, fmt.Sprintf("config %s", appName))
	require.NoError(t, err, "Failed to get config")

	// Check if the app has expected configuration (should have foo=bar)
	assert.Contains(t, configOutput, "foo", "foo config should be set")
	assert.Contains(t, configOutput, "bar", "foo config should be set to bar")

	// Check if app is running
	psOutput, err := ssh.CheckSshCommandE(t, host, fmt.Sprintf("ps:report %s", appName))
	require.NoError(t, err, "Failed to get process status")
	assert.Contains(t, psOutput, "Running", "App should be running")

	// Log the validation results
	t.Logf("App config: %s", configOutput)
	t.Logf("App status: %s", psOutput)
	t.Logf("App image info: %s", imageInfoOutput)
}

func destroyTerraform(t *testing.T, testDir string) {
	// Try to load and destroy terraform options
	defer func() {
		if r := recover(); r != nil {
			t.Logf("Terraform destroy failed: %v", r)
		}
	}()

	// Try to load terraform options
	terraformOptions := test_structure.LoadTerraformOptions(t, testDir)

	// Destroy the terraform resources
	terraform.Destroy(t, terraformOptions)
}

func cleanupDocker(t *testing.T, env *testEnvironment) {
	if env == nil || env.ContainerName == "" {
		t.Logf("No container to clean up")
		return
	}

	// Stop and remove container
	docker.Stop(t, []string{env.ContainerName}, &docker.StopOptions{})

	// Remove container (don't remove the official image)
	exec.Command("docker", "rm", "-f", env.ContainerName).Run()
	// Skip removing the official image: exec.Command("docker", "rmi", "-f", dokkuImageName).Run()

	t.Logf("Cleaned up container: %s", env.ContainerName)
}

func cleanupTestFiles(t *testing.T, testDir string) {
	// Remove ephemeral files generated during testing
	filesToRemove := []string{
		filepath.Join(testDir, ".terraformrc"),
		filepath.Join(testDir, "provider.tf"),
		filepath.Join(testDir, "variables.tf"),
		filepath.Join(testDir, "ssh_key"),
		filepath.Join(testDir, "ssh_key.pub"),
		filepath.Join(testDir, "terraform.tfstate"),
		filepath.Join(testDir, "terraform.tfstate.backup"),
		filepath.Join(testDir, "terraform-provider-dokku"),
		filepath.Join(testDir, ".terraform.lock.hcl"),
	}

	// Remove directories
	dirsToRemove := []string{
		filepath.Join(testDir, ".terraform"),
		filepath.Join(testDir, ".test-data"),
	}

	// Remove files first
	for _, file := range filesToRemove {
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			t.Logf("Warning: Could not remove file %s: %v", file, err)
		}
	}

	// Remove directories
	for _, dir := range dirsToRemove {
		if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
			t.Logf("Warning: Could not remove directory %s: %v", dir, err)
		}
	}

	t.Logf("Cleaned up ephemeral test files from: %s", testDir)
}
