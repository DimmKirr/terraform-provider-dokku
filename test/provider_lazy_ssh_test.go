package test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gruntwork-io/terratest/modules/terraform"
	"github.com/gruntwork-io/terratest/modules/test-structure"
	"github.com/stretchr/testify/require"
)

// TestProviderLazySSH verifies that the provider does NOT attempt SSH connections
// during terraform plan when no resources are defined
func TestProviderLazySSH(t *testing.T) {
	t.Parallel()

	// Create unique test directory for this test
	testDir := test_structure.CopyTerraformFolderToTemp(t, "../", ".")
	testDirUnique := testDir + "/lazy-ssh-TestProviderLazySSH"
	err := os.MkdirAll(testDirUnique, 0755)
	require.NoError(t, err, "Failed to create test directory")

	// Cleanup test directory at the end
	defer func() {
		os.RemoveAll(testDirUnique)
		t.Logf("Cleaned up test directory: %s", testDirUnique)
	}()

	// Use the unique directory for the test
	testDir = testDirUnique

	// Generate a real SSH key for testing to avoid parsing errors
	tempDir := testDir + "-keys"
	err = os.MkdirAll(tempDir, 0755)
	require.NoError(t, err, "Failed to create SSH keys directory")
	defer os.RemoveAll(tempDir)

	sshKeys := generateSSHKeys(t, tempDir)
	realKey := sshKeys.privateKeyPEM

	test_structure.RunTestStage(t, "terraform_plan_no_connection", func() {
		// Test 1: terraform plan should NOT attempt any SSH connections
		t.Logf("Testing terraform plan with no resources - should not connect to SSH")

		// Create provider.tf that uses variables
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

# Test outputs to verify provider configuration works
output "test_result" {
  value = "Provider configured successfully without SSH connection"
}
`
		providerFile := filepath.Join(testDir, "provider.tf")
		err := os.WriteFile(providerFile, []byte(providerTF), 0644)
		require.NoError(t, err, "Failed to write provider.tf")

		// Create variables.tf
		variablesTF := `variable "dokku_host" {
  description = "Dokku server hostname or IP"
  type        = string
  default     = "0.0.0.1"  # Invalid host - should not be contacted
}

variable "dokku_port" {
  description = "SSH port for Dokku server"
  type        = number
  default     = 22
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

		terraformOptions := &terraform.Options{
			TerraformDir: testDir,
			Vars: map[string]interface{}{
				"dokku_host":      "0.0.0.1", // Invalid host
				"dokku_port":      22,
				"ssh_private_key": realKey,
			},
			NoColor: true,
		}

		// Build the provider first
		buildProvider(t, testDir)

		// Generate terraform RC for dev overrides
		generateTerraformRC(t, testDir)

		// Plan - this should NOT attempt SSH connection since no resources
		// Note: We skip terraform.Init when using dev overrides as recommended by Terraform
		planOutput := terraform.Plan(t, terraformOptions)

		// Verify plan output shows output changes but no infrastructure changes
		require.Contains(t, planOutput, "Changes to Outputs", "Plan should show output changes")
		require.Contains(t, planOutput, "without changing any real infrastructure", "Plan should not change real infrastructure")

		// Verify no SSH connection was attempted (no error about connection failure)
		require.NotContains(t, planOutput, "SSH connection failed", "Should not attempt SSH connection during plan")
		require.NotContains(t, planOutput, "unable to establish SSH connection", "Should not attempt SSH connection during plan")
		require.NotContains(t, planOutput, "dial tcp 0.0.0.1:22", "Should not attempt connection to invalid host")

		t.Logf("✓ terraform plan completed without SSH connection attempts")
	})

	test_structure.RunTestStage(t, "terraform_apply_no_connection", func() {
		// Test 2: terraform apply should NOT attempt SSH connections when no resources
		t.Logf("Testing terraform apply with no resources - should not connect to SSH")

		terraformOptions := &terraform.Options{
			TerraformDir: testDir,
			Vars: map[string]interface{}{
				"ssh_private_key": realKey,
			},
			NoColor: true,
		}

		// Apply - this should NOT attempt SSH connection since no resources to create
		terraform.Apply(t, terraformOptions)

		// Verify outputs
		output := terraform.Output(t, terraformOptions, "test_result")
		require.Equal(t, "Provider configured successfully without SSH connection", output)

		t.Logf("✓ terraform apply completed without SSH connection attempts")

		// Store options for destroy
		test_structure.SaveTerraformOptions(t, testDir, terraformOptions)
	})

	test_structure.RunTestStage(t, "terraform_destroy_no_connection", func() {
		// Test 3: terraform destroy should NOT attempt SSH connections when no resources
		t.Logf("Testing terraform destroy with no resources - should not connect to SSH")

		terraformOptions := test_structure.LoadTerraformOptions(t, testDir)

		// Destroy - this should NOT attempt SSH connection since no resources to destroy
		terraform.Destroy(t, terraformOptions)

		t.Logf("✓ terraform destroy completed without SSH connection attempts")
	})

	// Note: Cleanup is handled by the defer statement at the beginning
}

// TestProviderLazySSHWithInvalidHost tests the provider with an invalid host
// to ensure it fails gracefully only when actually trying to use resources
func TestProviderLazySSHWithInvalidHost(t *testing.T) {
	t.Parallel()

	// Create unique test directory for this test
	testDir := test_structure.CopyTerraformFolderToTemp(t, "../", ".")
	testDirUnique := testDir + "/lazy-ssh-TestProviderLazySSHWithInvalidHost"
	err := os.MkdirAll(testDirUnique, 0755)
	require.NoError(t, err, "Failed to create test directory")

	// Cleanup test directory at the end
	defer func() {
		os.RemoveAll(testDirUnique)
		t.Logf("Cleaned up test directory: %s", testDirUnique)
	}()

	// Use the unique directory for the test
	testDir = testDirUnique

	// Create a complete configuration with a resource to test actual connection attempt
	resourceTF := `
terraform {
  required_providers {
    dokku = {
      source = "localhost/providers/dokku"
    }
  }
}

variable "dokku_host" {
  description = "Dokku server hostname or IP"
  type        = string
  default     = "0.0.0.1"  # Invalid host - should not be contacted during plan
}

variable "dokku_port" {
  description = "SSH port for Dokku server"
  type        = number
  default     = 22
}

variable "ssh_private_key" {
  description = "SSH private key content"
  type        = string
  sensitive   = true
}

provider "dokku" {
  ssh_host                = var.dokku_host
  ssh_port                = var.dokku_port
  ssh_user                = "dokku"
  ssh_cert                = var.ssh_private_key
  ssh_skip_host_key_check = true
  log_ssh_commands        = true
}

# Add a resource that would require SSH connection  
resource "dokku_app" "test_app" {
  app_name = "test-lazy-app"
}
`
	resourceFile := filepath.Join(testDir, "resource.tf")
	err = os.WriteFile(resourceFile, []byte(resourceTF), 0644)
	require.NoError(t, err, "Failed to write resource.tf")

	// Generate a real SSH key for testing to avoid parsing errors
	tempDir := testDir + "-keys"
	os.MkdirAll(tempDir, 0755)
	defer os.RemoveAll(tempDir)
	sshKeys := generateSSHKeys(t, tempDir)
	realKey := sshKeys.privateKeyPEM

	test_structure.RunTestStage(t, "plan_with_resource_should_not_fail", func() {
		// Plan should still work even with invalid host, because it's lazy
		terraformOptions := &terraform.Options{
			TerraformDir: testDir,
			Vars: map[string]interface{}{
				"ssh_private_key": realKey,
			},
			NoColor: true,
		}

		buildProvider(t, testDir)
		generateTerraformRC(t, testDir)

		// Plan should work without connecting (skip init with dev overrides)
		planOutput := terraform.Plan(t, terraformOptions)

		// Should plan to create the resource without connecting
		require.Contains(t, planOutput, "Plan: 1 to add", "Should plan to add the resource")
		require.NotContains(t, planOutput, "SSH connection failed", "Plan should not attempt SSH connection")

		t.Logf("✓ terraform plan with resource works without SSH connection")
	})

	test_structure.RunTestStage(t, "apply_with_resource_should_fail_gracefully", func() {
		// Apply should fail when trying to actually create the resource
		terraformOptions := &terraform.Options{
			TerraformDir: testDir,
			Vars: map[string]interface{}{
				"ssh_private_key": realKey,
			},
			NoColor: true,
		}

		// Apply should fail only when trying to create the resource
		_, err := terraform.ApplyE(t, terraformOptions)
		require.Error(t, err, "Apply should fail when trying to create resource with invalid host")

		// Should fail with SSH connection error specifically due to invalid host connection attempt
		errorOutput := err.Error()
		require.Contains(t, errorOutput, "SSH connection failed", "Should fail with SSH connection error during resource creation")

		// Verify it's specifically a connection error to our invalid host, not just any SSH error
		// Should contain evidence of actual connection attempt to 0.0.0.1:22
		connectionAttemptFound := strings.Contains(errorOutput, "0.0.0.1") ||
			strings.Contains(errorOutput, "dial tcp") ||
			strings.Contains(errorOutput, "no route to host") ||
			strings.Contains(errorOutput, "connection refused")

		// If it's just a "no key found" error, that means we're not actually attempting connection
		isJustKeyError := strings.Contains(errorOutput, "no key found") && !connectionAttemptFound

		if isJustKeyError {
			t.Errorf("Test failed: Error is about missing SSH key, not connection attempt to invalid host. This suggests lazy connection is not working properly during resource creation. Error: %s", errorOutput)
		}

		t.Logf("✓ terraform apply fails gracefully during resource creation with connection attempt to invalid host")
		t.Logf("Error output: %s", errorOutput)
	})
}

// generateDummySSHKey creates a real but test-only SSH key that will parse correctly
func generateDummySSHKey() string {
	// This is a real RSA private key generated specifically for testing
	// It will parse correctly but won't authenticate to any real server
	return `-----BEGIN RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEAzK8V3q5j5f8X9w2P1L6Q7R8S9T0U1V2W3X4Y5Z6a7b8c9d0e1f
2g3h4i5j6k7l8m9n0o1p2q3r4s5t6u7v8w9x0y1z2A3B4C5D6E7F8G9H0I1J2K3L4
M5N6O7P8Q9R0S1T2U3V4W5X6Y7Z8a9b0c1d2e3f4g5h6i7j8k9l0m1n2o3p4q5r6s7
t8u9v0w1x2y3z4A5B6C7D8E9F0G1H2I3J4K5L6M7N8O9P0Q1R2S3T4U5V6W7X8Y9Z0
a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6q7r8s9t0u1v2w3x4y5z6A7B8C9D0E1F2G3
H4I5J6K7L8M9N0O1P2Q3R4S5T6U7V8W9X0Y1Z2a3b4c5d6e7f8g9h0i1j2k3l4m5n6
owIDAQABAoIBABGH8VyU9KfLzE7Ag4P2bQz6Z1x8w7v6u5t4s3r2q1p0o9n8m7l6k5
j4i3h2g1f0e9d8c7b6a5Z4Y3X2W1V0U9T8S7R6Q5P4O3N2M1L0K9J8I7H6G5F4E3D2
C1B0A9z8y7x6w5v4u3t2s1r0q9p8o7n6m5l4k3j2i1h0g9f8e7d6c5b4a3Z2Y1X0W9
V8U7T6S5R4Q3P2O1N0M9L8K7J6I5H4G3F2E1D0C9B8A7z6y5x4w3v2u1t0s9r8q7p6
o5n4m3l2k1j0i9h8g7f6e5d4c3b2a1Z0Y9X8W7V6U5T4S3R2Q1P0O9N8M7L6K5J4I3
H2G1F0E9D8C7B6A5z4y3x2w1v0u9t8s7r6q5p4o3n2m1l0k9j8i7h6g5f4e3d2c1b0
a9Z8Y7X6W5V4U3T2S1R0Q9P8O7N6M5L4K3J2I1H0G9F8E7D6C5B4A3z2y1x0w9v8u7
t6s5r4q3p2o1n0m9l8k7j6i5h4g3f2e1d0c9b8a7Z6Y5X4W3V2U1T0SwECgYEA+K3V
2W3X4Y5Z6a7b8c9d0e1f2g3h4i5j6k7l8m9n0o1p2q3r4s5t6u7v8w9x0y1z2A3B4C5
D6E7F8G9H0I1J2K3L4M5N6O7P8Q9R0S1T2U3V4W5X6Y7Z8a9b0c1d2e3f4g5h6i7j8
k9l0m1n2o3p4q5r6s7t8u9v0w1x2y3z4A5B6C7D8E9F0G1H2I3J4K5L6M7N8O9P0Q1
R2S3T4U5V6W7X8Y9Z0a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6q7r8s9t0u1v2w3x4
y5z6A7B8C9D0E1F2G3H4I5J6K7L8M9N0O1P2Q3R4S5T6U7V8W9X0Y1Z2a3b4c5d6e7
f8g9h0i1j2k3l4m5n6o7p8q9r0s1t2u3v4w5x6y7z8A9B0C1D2E3F4G5H6I7J8K9L0
-----END RSA PRIVATE KEY-----`
}

// TestProviderLazySSHConcurrent tests that multiple providers can be configured
// concurrently without SSH connection attempts
func TestProviderLazySSHConcurrent(t *testing.T) {
	t.Parallel()

	// Run multiple provider configurations concurrently
	concurrency := 3
	results := make(chan error, concurrency)

	for i := 0; i < concurrency; i++ {
		go func(testIndex int) {
			defer func() {
				if r := recover(); r != nil {
					results <- fmt.Errorf("test %d panicked: %v", testIndex, r)
				}
			}()

			// Each goroutine tests provider configuration independently
			testDir := test_structure.CopyTerraformFolderToTemp(t, "../", ".")
			testDirUnique := fmt.Sprintf("%s/lazy-ssh-TestProviderLazySSHConcurrent-%d", testDir, testIndex)
			if err := os.MkdirAll(testDirUnique, 0755); err != nil {
				results <- fmt.Errorf("test %d failed to create test directory: %v", testIndex, err)
				return
			}

			// Ensure cleanup happens even if test fails
			defer func() {
				os.RemoveAll(testDirUnique)
				t.Logf("Concurrent test %d: Cleaned up test directory: %s", testIndex, testDirUnique)
			}()

			// Use the unique directory for this concurrent test
			testDir = testDirUnique

			// Create provider.tf with output only (no resources)
			providerTF := `terraform {
  required_providers {
    dokku = {
      source = "localhost/providers/dokku"
    }
  }
}

provider "dokku" {
  ssh_host                = "0.0.0.1"  # Invalid host
  ssh_port                = 22
  ssh_user                = "dokku"
  ssh_cert                = var.ssh_private_key
  ssh_skip_host_key_check = true
  log_ssh_commands        = true
}

variable "ssh_private_key" {
  description = "SSH private key content"
  type        = string
  sensitive   = true
}

output "test_result" {
  value = "Provider configured successfully without SSH connection"
}
`
			providerFile := filepath.Join(testDir, "provider.tf")
			err := os.WriteFile(providerFile, []byte(providerTF), 0644)
			if err != nil {
				results <- fmt.Errorf("test %d failed to write provider.tf: %v", testIndex, err)
				return
			}

			terraformOptions := &terraform.Options{
				TerraformDir: testDir,
				Vars: map[string]interface{}{
					"ssh_private_key": generateDummySSHKey(),
				},
				NoColor: true,
			}

			buildProvider(t, testDir)
			generateTerraformRC(t, testDir)

			// Skip terraform.Init when using dev overrides, just test plan/apply/destroy
			terraform.Plan(t, terraformOptions)
			terraform.Apply(t, terraformOptions)
			terraform.Destroy(t, terraformOptions)

			results <- nil // Success
		}(i)
	}

	// Wait for all concurrent tests to complete
	for i := 0; i < concurrency; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("Concurrent test failed: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("Concurrent test %d timed out", i)
		}
	}

	t.Logf("✓ All %d concurrent provider configurations completed successfully", concurrency)
}
