package test

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gruntwork-io/terratest/modules/ssh"
	"github.com/gruntwork-io/terratest/modules/terraform"
	"github.com/gruntwork-io/terratest/modules/test-structure"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSimple(t *testing.T) {
	t.Parallel()

	// Copy the specific test subdirectory
	sourceDir := filepath.Join("test", "simple")
	testDir := test_structure.CopyTerraformFolderToTemp(t, "../", sourceDir)

	// Create isolated test environment
	var env *testEnvironment
	test_structure.RunTestStage(t, "generate_ssh_keys", func() {
		env = createTestEnvironment(t, testDir)
	})

	// Ensure cleanup runs regardless of test outcome
	defer func() {
		if env != nil {
			cleanupTestEnvironment(t, env)
		}
	}()

	test_structure.RunTestStage(t, "setup_docker", func() {
		setupDokkuContainer(t, env)
	})

	test_structure.RunTestStage(t, "setup_ssh", func() {
		setupSSH(t, env)
	})

	test_structure.RunTestStage(t, "apply_terraform", func() {
		// Define test-specific configuration inline
		// Convert to lowercase and clean up the name for Dokku compatibility
		testName := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
		appName := fmt.Sprintf("simple-test-%s", testName)

		// Base terraform options using environment-specific settings
		sshPort, err := strconv.Atoi(env.ExternalPorts["ssh"])
		require.NoError(t, err, "Failed to parse SSH port")

		vars := map[string]interface{}{
			"dokku_host":      "localhost",
			"dokku_port":      sshPort,
			"ssh_private_key": env.SSHKeys.privateKeyPEM,
			"app_name":        appName,
		}

		terraformOptions := &terraform.Options{
			TerraformDir: testDir,
			Vars:         vars,
			NoColor:      true,
		}

		// Build the provider first
		buildProvider(t, testDir)

		// Generate terraform RC for dev overrides
		generateTerraformRC(t, testDir)

		// Apply directly since we have provider development overrides
		terraform.Apply(t, terraformOptions)

		// Store options for cleanup
		test_structure.SaveTerraformOptions(t, testDir, terraformOptions)
	})

	test_structure.RunTestStage(t, "validate_dokku", func() {
		// Convert to lowercase and clean up the name for Dokku compatibility
		testName := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
		appName := fmt.Sprintf("simple-test-%s", testName)

		keyPair := &ssh.KeyPair{
			PublicKey:  env.SSHKeys.publicKeySSH,
			PrivateKey: env.SSHKeys.privateKeyPEM,
		}

		// Convert external port string to int
		customPort, err := strconv.Atoi(env.ExternalPorts["ssh"])
		require.NoError(t, err, "Failed to parse SSH port")

		host := ssh.Host{
			Hostname:    "localhost",
			SshKeyPair:  keyPair,
			SshUserName: "dokku",
			CustomPort:  customPort,
		}

		validateSimpleApp(t, host, appName)
	})

	test_structure.RunTestStage(t, "validate_http", func() {
		// Test HTTP accessibility using the environment's external HTTP port
		maxRetries := 10
		retryInterval := 3 * time.Second

		var httpResponse string
		var httpErr error

		httpPort := env.ExternalPorts["http"]
		httpURL := fmt.Sprintf("http://localhost:%s", httpPort)

		for i := 0; i < maxRetries; i++ {
			// Use curl to test HTTP connectivity since the app should be accessible via the exposed port
			curlCmd := exec.Command("curl", "-s", "-f", httpURL)
			output, err := curlCmd.Output()

			if err == nil {
				httpResponse = string(output)
				httpErr = nil
				break
			}

			httpErr = err
			t.Logf("HTTP test attempt %d failed: %v", i+1, err)
			if i < maxRetries-1 {
				time.Sleep(retryInterval)
			}
		}

		require.NoError(t, httpErr, "Failed to get HTTP response from app after %d retries", maxRetries)

		// Verify the response contains expected content from jmalloc/echo-server
		assert.Contains(t, httpResponse, "Request served by", "HTTP response should contain 'Request served by' indicating the echo-server container is serving content")
		assert.Contains(t, httpResponse, fmt.Sprintf("Host: localhost:%s", httpPort), "HTTP response should show the app received the request on the correct port")

		t.Logf("HTTP validation successful! Response preview: %.200s...", httpResponse)
	})

	test_structure.RunTestStage(t, "destroy_terraform", func() {
		terraformOptions := test_structure.LoadTerraformOptions(t, testDir)
		terraform.Destroy(t, terraformOptions)
	})

	// Note: Final cleanup is handled by the defer statement at the beginning
	// which calls cleanupTestEnvironment(t, env)
}
