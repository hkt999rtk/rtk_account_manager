package main

import (
	"fmt"
	"io"
)

// Exact immutable images expose compiled capabilities before configuration,
// login, database access or listeners. The probe grants no deployment authority.
func writeDeploymentCapabilities(args []string, out io.Writer) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	if len(args) != 1 || args[0] != "--deployment-capabilities" {
		return true, fmt.Errorf("unsupported server arguments")
	}
	_, err := io.WriteString(out, "{\"schema\":\"rtk-cloud.deployment-capabilities/v1\",\"service\":\"account-manager\",\"features\":[\"deployment-bootstrap-finalize-v1\"]}\n")
	return true, err
}
