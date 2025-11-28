package test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gruntwork-io/terratest/modules/ssh"
	"github.com/gruntwork-io/terratest/modules/terraform"
	"github.com/gruntwork-io/terratest/modules/test-structure"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExampleDokkuAppBuilder(t *testing.T) {
	t.Parallel()

	// Create a temporary test directory
	testDir := test_structure.CopyTerraformFolderToTemp(t, "../", "test")

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
		// Disable detailed logging for cleaner output
		os.Unsetenv("TF_LOG")
		os.Unsetenv("TF_LOG_CORE")
		os.Unsetenv("TF_LOG_PROVIDER")

		// Generate provider config
		generateProviderConfig(t, testDir)

		// Create test Terraform config with builder
		mainTF := `
resource "dokku_app" "builder_test" {
  app_name = "builder-test"

  builder = {
    selected  = "dockerfile"
  }
}
`

		mainFile := filepath.Join(testDir, "main.tf")
		err := os.WriteFile(mainFile, []byte(mainTF), 0644)
		require.NoError(t, err, "Failed to write main.tf")

		// Generate variables
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
`

		variablesFile := filepath.Join(testDir, "variables.tf")
		err = os.WriteFile(variablesFile, []byte(variablesTF), 0644)
		require.NoError(t, err, "Failed to write variables.tf")

		// Base terraform options using environment-specific settings
		sshPort, err := strconv.Atoi(env.ExternalPorts["ssh"])
		require.NoError(t, err, "Failed to parse SSH port")

		vars := map[string]interface{}{
			"dokku_host":      "localhost",
			"dokku_port":      sshPort,
			"ssh_private_key": env.SSHKeys.privateKeyPEM,
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

		// Apply
		terraform.InitAndApply(t, terraformOptions)

		// Store options for cleanup
		test_structure.SaveTerraformOptions(t, testDir, terraformOptions)
	})

	test_structure.RunTestStage(t, "validate_builder_selected", func() {
		serviceName := "builder-test"

		keyPair := &ssh.KeyPair{
			PublicKey:  env.SSHKeys.publicKeySSH,
			PrivateKey: env.SSHKeys.privateKeyPEM,
		}

		customPort, err := strconv.Atoi(env.ExternalPorts["ssh"])
		require.NoError(t, err, "Failed to parse SSH port")

		host := ssh.Host{
			Hostname:    "localhost",
			SshKeyPair:  keyPair,
			SshUserName: "dokku",
			CustomPort:  customPort,
		}

		t.Logf("Validating builder configuration for: %s", serviceName)

		// Check if the app exists
		appOutput, err := ssh.CheckSshCommandE(t, host, "apps:list")
		require.NoError(t, err, "Failed to get app list")
		assert.Contains(t, appOutput, serviceName, "App should be created")

		// Check builder:report output
		builderOutput, err := ssh.CheckSshCommandE(t, host, fmt.Sprintf("builder:report %s", serviceName))
		require.NoError(t, err, "Failed to get builder report")

		t.Logf("Builder report: %s", builderOutput)

		// Verify builder selected is set
		assert.Contains(t, builderOutput, "Builder selected:", "Should have Builder selected field")
		assert.Contains(t, builderOutput, "dockerfile", "Builder selected should be dockerfile")
	})

	test_structure.RunTestStage(t, "test_builder_update", func() {
		serviceName := "builder-test"

		// Update the Terraform config to change builder
		mainTF := `
resource "dokku_app" "builder_test" {
  app_name = "builder-test"

  builder = {
    selected  = "herokuish"  # Changed from dockerfile
  }
}
`

		mainFile := filepath.Join(testDir, "main.tf")
		err := os.WriteFile(mainFile, []byte(mainTF), 0644)
		require.NoError(t, err, "Failed to write updated main.tf")

		// Re-load terraform options
		terraformOptions := test_structure.LoadTerraformOptions(t, testDir)

		// Apply changes
		terraform.Apply(t, terraformOptions)

		// Verify via SSH
		keyPair := &ssh.KeyPair{
			PublicKey:  env.SSHKeys.publicKeySSH,
			PrivateKey: env.SSHKeys.privateKeyPEM,
		}

		customPort, err := strconv.Atoi(env.ExternalPorts["ssh"])
		require.NoError(t, err, "Failed to parse SSH port")

		host := ssh.Host{
			Hostname:    "localhost",
			SshKeyPair:  keyPair,
			SshUserName: "dokku",
			CustomPort:  customPort,
		}

		builderOutput, err := ssh.CheckSshCommandE(t, host, fmt.Sprintf("builder:report %s", serviceName))
		require.NoError(t, err, "Failed to get builder report after update")

		t.Logf("Builder report after update: %s", builderOutput)

		// Verify builder was updated
		assert.Contains(t, builderOutput, "herokuish", "Builder selected should be herokuish after update")
	})

	test_structure.RunTestStage(t, "test_build_dir", func() {
		// Update config to add build_dir
		mainTF := `
resource "dokku_app" "builder_test" {
  app_name = "builder-test"

  builder = {
    selected  = "dockerfile"
    build_dir = "api"  # Added
  }
}
`

		mainFile := filepath.Join(testDir, "main.tf")
		err := os.WriteFile(mainFile, []byte(mainTF), 0644)
		require.NoError(t, err, "Failed to write main.tf with build_dir")

		terraformOptions := test_structure.LoadTerraformOptions(t, testDir)
		terraform.Apply(t, terraformOptions)

		// Verify via SSH
		keyPair := &ssh.KeyPair{
			PublicKey:  env.SSHKeys.publicKeySSH,
			PrivateKey: env.SSHKeys.privateKeyPEM,
		}

		customPort, err := strconv.Atoi(env.ExternalPorts["ssh"])
		require.NoError(t, err, "Failed to parse SSH port")

		host := ssh.Host{
			Hostname:    "localhost",
			SshKeyPair:  keyPair,
			SshUserName: "dokku",
			CustomPort:  customPort,
		}

		builderOutput, err := ssh.CheckSshCommandE(t, host, "builder:report builder-test")
		require.NoError(t, err, "Failed to get builder report with build_dir")

		t.Logf("Builder report with build_dir: %s", builderOutput)

		// Verify build_dir was set
		assert.Contains(t, builderOutput, "Builder build dir:", "Should have Builder build dir field")
		assert.Contains(t, builderOutput, "api", "Build dir should be 'api'")
	})

	test_structure.RunTestStage(t, "test_remove_builder", func() {
		// Remove builder block
		mainTF := `
resource "dokku_app" "builder_test" {
  app_name = "builder-test"
  # builder block removed
}
`

		mainFile := filepath.Join(testDir, "main.tf")
		err := os.WriteFile(mainFile, []byte(mainTF), 0644)
		require.NoError(t, err, "Failed to write main.tf without builder")

		terraformOptions := test_structure.LoadTerraformOptions(t, testDir)
		terraform.Apply(t, terraformOptions)

		// Verify builder settings are cleared
		keyPair := &ssh.KeyPair{
			PublicKey:  env.SSHKeys.publicKeySSH,
			PrivateKey: env.SSHKeys.privateKeyPEM,
		}

		customPort, err := strconv.Atoi(env.ExternalPorts["ssh"])
		require.NoError(t, err, "Failed to parse SSH port")

		host := ssh.Host{
			Hostname:    "localhost",
			SshKeyPair:  keyPair,
			SshUserName: "dokku",
			CustomPort:  customPort,
		}

		builderOutput, err := ssh.CheckSshCommandE(t, host, "builder:report builder-test")
		require.NoError(t, err, "Failed to get builder report after removal")

		t.Logf("Builder report after removal: %s", builderOutput)

		// Check that builder settings are empty
		// The output format is "Builder selected:              " with trailing spaces when empty
		lines := strings.Split(builderOutput, "\n")
		for _, line := range lines {
			if strings.Contains(line, "Builder selected:") {
				// Extract value after colon
				parts := strings.Split(line, ":")
				if len(parts) >= 2 {
					value := strings.TrimSpace(parts[1])
					assert.Empty(t, value, "Builder selected should be empty after removal")
				}
			}
			if strings.Contains(line, "Builder build dir:") {
				parts := strings.Split(line, ":")
				if len(parts) >= 2 {
					value := strings.TrimSpace(parts[1])
					assert.Empty(t, value, "Builder build dir should be empty after removal")
				}
			}
		}
	})

	// Note: Final cleanup is handled by the defer statement at the beginning
	// which calls cleanupTestEnvironment(t, env)
}
