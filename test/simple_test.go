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

		// Add extra debugging to inspect container networks
		dokkuContainerName := env.ContainerName
		dokkuInspectCmd := exec.Command("docker", "inspect", "-f", "'{{.NetworkSettings.Networks}}'", dokkuContainerName)
		dokkuNet, _ := dokkuInspectCmd.CombinedOutput()
		t.Logf("Dokku container network: %s", string(dokkuNet))

		appInspectCmd := exec.Command("docker", "inspect", "-f", "'{{.NetworkSettings.Networks}}'", fmt.Sprintf("%s.web.1", appName))
		appNet, _ := appInspectCmd.CombinedOutput()
		t.Logf("App container network: %s", string(appNet))

		validateSimpleApp(t, host, appName)
	})

	test_structure.RunTestStage(t, "debug_containers", func() {
		terraformOptions := test_structure.LoadTerraformOptions(t, testDir)
		appName := terraform.Output(t, terraformOptions, "app_name")

		t.Logf("=== DEBUGGING CONTAINER STATES AND NETWORKING ===")

		// 0. Network Analysis for Troubleshooting
		t.Logf("--- Network Analysis ---")

		// List all Docker networks
		networksCmd := exec.Command("docker", "network", "ls")
		networks, err := networksCmd.CombinedOutput()
		if err != nil {
			t.Logf("Failed to list Docker networks: %v", err)
		} else {
			t.Logf("Docker networks: %s", string(networks))
		}

		// Check if containers are on the same network
		dokkuNetworkCmd := exec.Command("docker", "inspect", env.ContainerName, "--format", "{{range $net, $v := .NetworkSettings.Networks}}{{$net}} {{end}}")
		dokkuNetworks, err := dokkuNetworkCmd.CombinedOutput()
		if err != nil {
			t.Logf("Failed to get Dokku container networks: %v", err)
		} else {
			t.Logf("Dokku container networks: %s", string(dokkuNetworks))
		}

		appContainerName := fmt.Sprintf("%s.web.1", appName)
		appNetworkCmd := exec.Command("docker", "inspect", appContainerName, "--format", "{{range $net, $v := .NetworkSettings.Networks}}{{$net}} {{end}}")
		appNetworks, err := appNetworkCmd.CombinedOutput()
		if err != nil {
			t.Logf("Failed to get app container networks: %v", err)
		} else {
			t.Logf("App container networks: %s", string(appNetworks))
		}

		// Check if they can ping each other by name (network connectivity test)
		dokkuToAppPingCmd := exec.Command("docker", "exec", env.ContainerName, "ping", "-c", "1", "-W", "2", appContainerName)
		dokkuToAppPing, err := dokkuToAppPingCmd.CombinedOutput()
		if err != nil {
			t.Logf("Dokku->App ping failed: %v, output: %s", err, string(dokkuToAppPing))
		} else {
			t.Logf("Dokku->App ping successful: %s", string(dokkuToAppPing))
		}

		// 1. Check Dokku container port bindings and network
		t.Logf("--- Dokku Container Inspection ---")
		dokkuInspectCmd := exec.Command("docker", "inspect", env.ContainerName, "--format",
			"{{.State.Status}} | {{.NetworkSettings.Ports}} | {{.NetworkSettings.IPAddress}} | {{.NetworkSettings.Networks}}")
		dokkuInspect, err := dokkuInspectCmd.CombinedOutput()
		if err != nil {
			t.Logf("Dokku container inspect failed: %v", err)
		} else {
			t.Logf("Dokku container state: %s", string(dokkuInspect))
		}

		// 2. Check app container existence and state
		t.Logf("--- App Container Inspection ---")
		appInspectCmd := exec.Command("docker", "inspect", appContainerName, "--format",
			"{{.State.Status}} | {{.NetworkSettings.Ports}} | {{.NetworkSettings.IPAddress}} | {{.NetworkSettings.Networks}}")
		appInspect, err := appInspectCmd.CombinedOutput()
		if err != nil {
			t.Logf("App container inspect failed: %v", err)
		} else {
			t.Logf("App container state: %s", string(appInspect))
		}

		// 3. Check if app container is listening on expected port
		t.Logf("--- App Container Port Check ---")
		portCheckCmd := exec.Command("docker", "exec", appContainerName, "netstat", "-tlnp")
		portCheck, err := portCheckCmd.CombinedOutput()
		if err != nil {
			t.Logf("App container port check failed: %v", err)
			// Try alternative command
			altPortCheckCmd := exec.Command("docker", "exec", appContainerName, "ss", "-tlnp")
			altPortCheck, err2 := altPortCheckCmd.CombinedOutput()
			if err2 != nil {
				t.Logf("App container alternative port check also failed: %v", err2)
			} else {
				t.Logf("App container ports (ss): %s", string(altPortCheck))
			}
		} else {
			t.Logf("App container ports (netstat): %s", string(portCheck))
		}

		// 4. Test direct connection to app container from Dokku container
		t.Logf("--- Direct Container-to-Container Test ---")
		appInternalIP := ""
		getIPCmd := exec.Command("docker", "inspect", appContainerName, "--format", "{{.NetworkSettings.IPAddress}}")
		ipOutput, err := getIPCmd.CombinedOutput()
		if err == nil {
			appInternalIP = strings.TrimSpace(string(ipOutput))
			t.Logf("App container internal IP: %s", appInternalIP)

			if appInternalIP != "" {
				// Test connection from Dokku container to app container
				directTestCmd := exec.Command("docker", "exec", env.ContainerName, "curl", "-s", "-m", "5",
					fmt.Sprintf("http://%s:5000", appInternalIP))
				directTest, err := directTestCmd.CombinedOutput()
				if err != nil {
					t.Logf("Direct container connection failed: %v", err)
				} else {
					response := string(directTest)
					if len(response) > 200 {
						response = response[:200]
					}
					t.Logf("Direct container connection successful: %s", response)
				}
			}
		}

		// 5. Check nginx configuration in Dokku container
		t.Logf("--- Nginx Configuration Check ---")
		nginxCheckCmd := exec.Command("docker", "exec", env.ContainerName, "ls", "-la", fmt.Sprintf("/home/dokku/%s/nginx.conf.d/", appName))
		nginxCheck, err := nginxCheckCmd.CombinedOutput()
		if err != nil {
			t.Logf("Nginx config dir check failed: %v", err)
		} else {
			t.Logf("Nginx config dir: %s", string(nginxCheck))
		}

		// Check main nginx config
		nginxMainCmd := exec.Command("docker", "exec", env.ContainerName, "cat", fmt.Sprintf("/home/dokku/%s/nginx.conf", appName))
		nginxMain, err := nginxMainCmd.CombinedOutput()
		if err != nil {
			t.Logf("Nginx main config read failed: %v", err)
		} else {
			t.Logf("Nginx main config: %s", string(nginxMain))
		}

		// 6. Check if nginx is listening on port 80 in Dokku container
		t.Logf("--- Dokku Container Nginx Port Check ---")
		dokkuPortCheckCmd := exec.Command("docker", "exec", env.ContainerName, "netstat", "-tlnp")
		dokkuPortCheck, err := dokkuPortCheckCmd.CombinedOutput()
		if err != nil {
			t.Logf("Dokku container port check failed: %v", err)
		} else {
			t.Logf("Dokku container ports: %s", string(dokkuPortCheck))
		}

		// 7. Test nginx from inside Dokku container with comprehensive debugging
		t.Logf("--- Internal Nginx Test ---")
		hostHeaderInternal := fmt.Sprintf("Host: %s.dokku.test", appName)
		t.Logf("Testing internal nginx with URL: http://localhost, Host Header: %s", hostHeaderInternal)

		// First check if nginx is running inside the container
		nginxStatusCmd := exec.Command("docker", "exec", env.ContainerName, "ps", "aux")
		nginxStatus, err := nginxStatusCmd.CombinedOutput()
		if err != nil {
			t.Logf("Failed to check processes in container: %v", err)
		} else {
			nginxLines := strings.Split(string(nginxStatus), "\n")
			nginxFound := false
			for _, line := range nginxLines {
				if strings.Contains(line, "nginx") {
					t.Logf("Nginx process found: %s", line)
					nginxFound = true
				}
			}
			if !nginxFound {
				t.Logf("WARNING: No nginx processes found in container!")
			}
		}

		// Check nginx configuration files
		nginxConfigCmd := exec.Command("docker", "exec", env.ContainerName, "find", "/etc/nginx", "-name", "*.conf", "-type", "f")
		nginxConfig, err := nginxConfigCmd.CombinedOutput()
		if err != nil {
			t.Logf("Failed to find nginx config files: %v", err)
		} else {
			t.Logf("Nginx config files found: %s", string(nginxConfig))
		}

		// Check if nginx is listening on port 80
		netstatCmd := exec.Command("docker", "exec", env.ContainerName, "netstat", "-tlnp")
		netstat, err := netstatCmd.CombinedOutput()
		if err != nil {
			t.Logf("Netstat failed, trying ss: %v", err)
			ssCmd := exec.Command("docker", "exec", env.ContainerName, "ss", "-tlnp")
			ss, ssErr := ssCmd.CombinedOutput()
			if ssErr != nil {
				t.Logf("Both netstat and ss failed: %v", ssErr)
			} else {
				t.Logf("Port listening check (ss): %s", string(ss))
			}
		} else {
			portLines := strings.Split(string(netstat), "\n")
			port80Found := false
			for _, line := range portLines {
				if strings.Contains(line, ":80 ") || strings.Contains(line, ":80\t") {
					t.Logf("Port 80 listener found: %s", line)
					port80Found = true
				}
			}
			if !port80Found {
				t.Logf("WARNING: No process listening on port 80!")
			}
		}

		// Now test the actual HTTP connection with detailed curl debugging
		internalNginxCmd := exec.Command("docker", "exec", env.ContainerName, "curl", "-v", "-s", "-m", "10",
			"-w", "CURL_RESULT: HTTP_CODE:%{http_code}|TOTAL_TIME:%{time_total}|CONNECT_TIME:%{time_connect}|SIZE:%{size_download}",
			"-H", hostHeaderInternal, "http://localhost")
		internalNginx, err := internalNginxCmd.CombinedOutput()

		if err != nil {
			var exitCode int
			if exitError, ok := err.(*exec.ExitError); ok {
				exitCode = exitError.ExitCode()
			}

			curlErrorMsg := ""
			switch exitCode {
			case 6:
				curlErrorMsg = " (Could not resolve host)"
			case 7:
				curlErrorMsg = " (Failed to connect to host)"
			case 22:
				curlErrorMsg = " (HTTP response code >= 400)"
			case 28:
				curlErrorMsg = " (Operation timeout)"
			case 52:
				curlErrorMsg = " (Empty reply from server)"
			case 56:
				curlErrorMsg = " (Failure in receiving network data)"
			default:
				curlErrorMsg = " (Other curl error)"
			}

			t.Logf("Internal nginx test failed: curl exit code %d, error: %v%s", exitCode, err, curlErrorMsg)
			t.Logf("Curl output: %s", string(internalNginx))

			// Try a simple connection test without Host header
			simpleTestCmd := exec.Command("docker", "exec", env.ContainerName, "curl", "-s", "-m", "5", "http://localhost")
			simpleTest, simpleErr := simpleTestCmd.CombinedOutput()
			if simpleErr != nil {
				t.Logf("Simple localhost test also failed: %v", simpleErr)
			} else {
				t.Logf("Simple localhost test succeeded: %s", string(simpleTest))
			}
		} else {
			response := string(internalNginx)
			if len(response) > 500 {
				response = response[:500]
			}
			t.Logf("Internal nginx test successful: %s", response)
		}

		// 8. Check host port binding
		t.Logf("--- Host Port Binding Check ---")
		httpPort := env.ExternalPorts["http"]
		hostPortCmd := exec.Command("netstat", "-tlnp")
		hostPort, err := hostPortCmd.CombinedOutput()
		if err != nil {
			t.Logf("Host port check failed: %v", err)
		} else {
			portLines := strings.Split(string(hostPort), "\n")
			for _, line := range portLines {
				if strings.Contains(line, fmt.Sprintf(":%s", httpPort)) {
					t.Logf("Host port binding found: %s", line)
				}
			}
		}

		t.Logf("=== END DEBUGGING ===")
	})

	test_structure.RunTestStage(t, "validate_http", func() {
		terraformOptions := test_structure.LoadTerraformOptions(t, testDir)
		// Test HTTP accessibility using the environment's external HTTP port
		maxRetries := 10
		retryInterval := 3 * time.Second

		var httpResponse string
		var httpErr error

		httpPort := env.ExternalPorts["http"]
		appName := terraform.Output(t, terraformOptions, "app_name")
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

		require.NoError(t, httpErr, "Failed to get HTTP response from app after %d retries", maxRetries)

		// Verify the response contains expected content from jmalloc/echo-server
		assert.Contains(t, httpResponse, "Request served by", "HTTP response should contain 'Request served by' indicating the echo-server container is serving content")
		assert.Contains(t, httpResponse, fmt.Sprintf("Host: %s.dokku.test", appName), "HTTP response should show the app received the request with the correct host header")

		t.Logf("HTTP validation successful! Response preview: %.200s...", httpResponse)
	})

	test_structure.RunTestStage(t, "destroy_terraform", func() {
		terraformOptions := test_structure.LoadTerraformOptions(t, testDir)
		terraform.Destroy(t, terraformOptions)
	})

	// Note: Final cleanup is handled by the defer statement at the beginning
	// which calls cleanupTestEnvironment(t, env)
}
