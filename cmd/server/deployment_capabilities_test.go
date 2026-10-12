package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
)

func TestNativeDeploymentCapabilityModePrecedesConfiguration(t *testing.T) {
	if mode := os.Getenv("RTK_CAPABILITY_MAIN_FIXTURE"); mode != "" {
		os.Args = []string{"server", mode}
		main()
		os.Exit(0)
	}
	for _, mode := range []string{"--deployment-capabilities", "--unknown"} {
		command := exec.Command(os.Args[0], "-test.run=^TestNativeDeploymentCapabilityModePrecedesConfiguration$")
		command.Env = append(os.Environ(), "RTK_CAPABILITY_MAIN_FIXTURE="+mode, "DATABASE_URL=invalid-private-fixture", "PKI_CONTROLLER_URL=invalid-private-fixture")
		output, err := command.CombinedOutput()
		if mode == "--deployment-capabilities" {
			var result map[string]any
			if err != nil || json.Unmarshal(output, &result) != nil || result["service"] != "account-manager" {
				t.Fatal("native early capability mode read privileged configuration", err, string(output))
			}
		} else if failure, ok := err.(*exec.ExitError); !ok || failure.ExitCode() != 2 || !bytes.Contains(output, []byte("unsupported server arguments")) {
			t.Fatal("unknown flag entered privileged runtime", err, string(output))
		}
	}
}

func TestDeploymentCapabilitiesBypassRuntimeAndRejectOtherArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"--deployment-capabilities"}, {"--unknown"}, {"--deployment-capabilities", "--unknown"}, {"--deployment-capabilities=true"}} {
		var output bytes.Buffer
		handled, err := writeDeploymentCapabilities(args, &output)
		if len(args) == 0 {
			if handled || err != nil || output.Len() != 0 {
				t.Fatal("ordinary runtime invocation changed")
			}
			continue
		}
		if !handled {
			t.Fatal("unknown arguments could enter configured privileged runtime")
		}
		if len(args) != 1 || args[0] != "--deployment-capabilities" {
			if err == nil || output.Len() != 0 {
				t.Fatal("invalid flag published a capability or entered runtime")
			}
			continue
		}
		var result struct {
			Schema, Service string
			Features        []string
		}
		if err != nil || json.Unmarshal(output.Bytes(), &result) != nil || result.Schema != "rtk-cloud.deployment-capabilities/v1" || result.Service != "account-manager" || len(result.Features) != 1 || result.Features[0] != "deployment-bootstrap-finalize-v1" {
			t.Fatal("native compiled feature contract is invalid", err, output.String())
		}
	}
}
