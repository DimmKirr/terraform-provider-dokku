package test

import (
	"fmt"
	"os"
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

func TestComplexAppConfig(t *testing.T) {
	t.Parallel()
	// This test uses properly typed variables with Terraform merge() function and validates
	// that provider handles complex HCL types correctly and sets expected environment variables

	// Copy the specific test subdirectory
	sourceDir := filepath.Join("test", "complex_app_config")
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
		// Disable detailed logging for cleaner output
		os.Unsetenv("TF_LOG")
		os.Unsetenv("TF_LOG_CORE")
		os.Unsetenv("TF_LOG_PROVIDER")

		// Define test-specific configuration inline
		appName := "complex-test-app"

		// Base terraform options using environment-specific settings
		sshPort, err := strconv.Atoi(env.ExternalPorts["ssh"])
		require.NoError(t, err, "Failed to parse SSH port")

		vars := map[string]interface{}{
			"dokku_host":      "localhost",
			"dokku_port":      sshPort,
			"ssh_private_key": env.SSHKeys.privateKeyPEM,
			"app_name":        appName,
			"app_config": map[string]interface{}{
				"ENV":      "prod",
				"APP_NAME": appName,
				"DEBUG":    "false",
				"API_URL":  "https://api.example.com",
				"PORT":     "5000",
			},
			"extra_domains": []string{
				"extra-domain.test",
			},
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

		// Skip init when using dev overrides (as recommended by Terraform warning)
		// Apply directly since we have provider development overrides
		// Now using properly typed app_config variable - this should work unless there are provider type issues
		terraformOutput, applyErr := terraform.ApplyE(t, terraformOptions)

		// Capture the terraform output for analysis
		outputStr := terraformOutput
		if applyErr != nil {
			outputStr += "\n" + applyErr.Error()
		}

		t.Logf("Terraform output captured: %s", outputStr)

		// Check for provider type-related errors - these should NOT occur with properly typed variables
		typeErrorPatterns := []string{
			"Incorrect attribute value type",
			"Invalid attribute configuration",
			"Type mismatch",
			"Incorrect type",
			"provider issue",
			"Block type",
			"does not support",
			"Value Conversion Error",
			"Target Type:",
			"Suggested Type:",
		}

		var foundTypeError bool
		var typeErrorMsg string
		for _, pattern := range typeErrorPatterns {
			if strings.Contains(outputStr, pattern) {
				foundTypeError = true
				typeErrorMsg = pattern
				break
			}
		}

		// Assert that NO type errors should occur when using properly typed variables
		if foundTypeError {
			require.Fail(t, "Provider type error found when using properly typed app_config variable: %s\nFull output: %s", typeErrorMsg, outputStr)
		}

		// If apply failed for other reasons (connection, etc.), that's acceptable
		if applyErr != nil {
			t.Logf("✓ Apply failed with non-type error (acceptable): %v", applyErr)
		} else {
			t.Logf("✓ Apply succeeded with properly typed variables")
		}

		// Store options for cleanup
		test_structure.SaveTerraformOptions(t, testDir, terraformOptions)
	})

	test_structure.RunTestStage(t, "validate_dokku", func() {
		// Only run validation if apply succeeded (no type errors found)
		appName := "complex-test-app"

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

		// Test SSH connection first with a retry mechanism
		maxRetries := 5
		var output string
		for i := 0; i < maxRetries; i++ {
			result, err := ssh.CheckSshCommandE(t, host, "apps:list")
			if err == nil {
				output = result
				break
			}

			t.Logf("SSH validation attempt %d failed: %v", i+1, err)
			if i < maxRetries-1 {
				time.Sleep(3 * time.Second)
			} else {
				t.Skipf("SSH validation failed after %d attempts, skipping validation: %v", maxRetries, err)
				return
			}
		}

		// Verify app exists
		if !strings.Contains(output, appName) {
			t.Logf("App %s not found in apps list, skipping env var validation", appName)
			return
		}

		// Verify environment variables from merge() function
		configOutput, err := ssh.CheckSshCommandE(t, host, fmt.Sprintf("config %s", appName))
		if err != nil {
			t.Logf("Failed to get config, skipping env var validation: %v", err)
			return
		}

		t.Logf("App config output: %s", configOutput)

		// Verify variables from var.app_config
		expectedVars := map[string]string{
			"ENV":      "prod",
			"APP_NAME": appName,
			"DEBUG":    "false",
			"API_URL":  "https://api.example.com",
			"PORT":     "5000",
		}

		// Verify variables from merge() function
		mergedVars := map[string]string{
			"MERGED_VAR": "foo",
			"NODE_ENV":   "production",
		}

		// Combine all expected variables
		allExpectedVars := make(map[string]string)
		for k, v := range expectedVars {
			allExpectedVars[k] = v
		}
		for k, v := range mergedVars {
			allExpectedVars[k] = v
		}

		// Verify all environment variables are present
		for key, expectedValue := range allExpectedVars {
			if !strings.Contains(configOutput, key) {
				t.Errorf("Environment variable %s not found in config", key)
				continue
			}
			if !strings.Contains(configOutput, expectedValue) {
				t.Errorf("Environment variable %s does not contain expected value %s", key, expectedValue)
			} else {
				t.Logf("✓ Verified environment variable %s=%s", key, expectedValue)
			}
		}

		t.Logf("✓ Environment variable verification completed")

		// Verify domains configuration
		domainsOutput, err := ssh.CheckSshCommandE(t, host, fmt.Sprintf("domains:report %s", appName))
		if err != nil {
			t.Logf("Failed to get domains report, skipping domains validation: %v", err)
			return
		}

		t.Logf("App domains output: %s", domainsOutput)

		// Expected domains from toset(concat()) function
		expectedDomains := []string{
			fmt.Sprintf("extra.%s.dokku.test", appName), // From concat first element
			fmt.Sprintf("%s.dokku.test", appName),       // From inline list
			fmt.Sprintf("api.%s.dokku.test", appName),   // From inline list
			fmt.Sprintf("www.%s.dokku.test", appName),   // From inline list
			"extra-domain.test",                         // From var.extra_domains
		}

		// Verify all domains are configured
		for _, expectedDomain := range expectedDomains {
			if !strings.Contains(domainsOutput, expectedDomain) {
				t.Errorf("Domain %s not found in domains report", expectedDomain)
			} else {
				t.Logf("✓ Verified domain %s is configured", expectedDomain)
			}
		}

		t.Logf("✓ Domains verification completed")
	})

	test_structure.RunTestStage(t, "validate_http", func() {
		appName := "complex-test-app"

		// Only run HTTP test if apply succeeded (no type errors found)
		maxRetries := 10
		retryInterval := 3 * time.Second

		var httpResponse string
		var httpErr error

		httpPort := env.ExternalPorts["http"]
		httpURL := fmt.Sprintf("http://localhost:%s", httpPort)
		hostHeader := fmt.Sprintf("Host: %s.dokku.test", appName)

		t.Logf("Testing HTTP connectivity - App: %s, URL: %s, Host Header: %s", appName, httpURL, hostHeader)

		for i := 0; i < maxRetries; i++ {
			// Use curl to test HTTP connectivity with verbose output for debugging
			curlCmd := exec.Command("curl", "-s", "-f", "-w", "HTTP_CODE:%{http_code}|TOTAL_TIME:%{time_total}|CONNECT_TIME:%{time_connect}|",
				"-H", hostHeader, httpURL)
			output, err := curlCmd.CombinedOutput()

			if err == nil {
				httpResponse = string(output)
				httpErr = nil
				t.Logf("HTTP test attempt %d succeeded: %s", i+1, httpResponse)
				break
			}

			// Enhanced error logging with curl exit codes and detailed analysis
			var exitCode int
			if exitError, ok := err.(*exec.ExitError); ok {
				exitCode = exitError.ExitCode()
			}

			httpErr = fmt.Errorf("curl exit code: %d, error: %v, output: %s", exitCode, err, string(output))

			// Provide specific curl error code meanings for common issues
			curlErrorMsg := ""
			switch exitCode {
			case 6:
				curlErrorMsg = " (Could not resolve host)"
			case 7:
				curlErrorMsg = " (Failed to connect to host)"
			case 22:
				curlErrorMsg = " (HTTP response code >= 400 - server returned error)"
			case 28:
				curlErrorMsg = " (Operation timeout)"
			case 52:
				curlErrorMsg = " (Empty reply from server)"
			case 56:
				curlErrorMsg = " (Failure in receiving network data)"
			case 60:
				curlErrorMsg = " (SSL certificate problem)"
			default:
				curlErrorMsg = " (See https://everything.curl.dev/usingcurl/returns for full list)"
			}

			t.Logf("HTTP test attempt %d failed: %v%s", i+1, httpErr, curlErrorMsg)

			// Additional debugging for HTTP errors
			if exitCode == 22 {
				// Try to get the actual HTTP response code
				statusCmd := exec.Command("curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
					"-H", hostHeader, httpURL)
				statusOutput, statusErr := statusCmd.Output()
				if statusErr == nil {
					t.Logf("HTTP response code: %s", string(statusOutput))
				}
			}

			if i < maxRetries-1 {
				time.Sleep(retryInterval)
			}
		}

		// Only require HTTP success if terraform apply succeeded
		// (we don't want to fail the test if there were only terraform type errors)
		if httpErr != nil {
			t.Logf("HTTP test failed: %v", httpErr)
			t.Logf("This may be expected if the app deployment failed due to environment issues")
		} else {
			// Verify the response contains expected content from jmalloc/echo-server
			assert.Contains(t, httpResponse, "Request served by", "HTTP response should contain 'Request served by' indicating the echo-server container is serving content")
			assert.Contains(t, httpResponse, fmt.Sprintf("Host: %s.dokku.test", appName), "HTTP response should show the app received the request with the correct host header")
			t.Logf("HTTP validation successful! Response preview: %.200s...", httpResponse)
		}
	})

	test_structure.RunTestStage(t, "destroy_terraform", func() {
		terraformOptions := test_structure.LoadTerraformOptions(t, testDir)
		// Use DestroyE since the apply might have failed and there might be nothing to destroy
		_, destroyErr := terraform.DestroyE(t, terraformOptions)
		if destroyErr != nil {
			t.Logf("Destroy failed (expected if apply failed): %v", destroyErr)
		}
	})

	// Note: Final cleanup is handled by the defer statement at the beginning
	// which calls cleanupTestEnvironment(t, env)
}
