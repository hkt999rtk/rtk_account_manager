package manifest

import (
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Manifest struct {
	Stack     string                     `yaml:"stack"`
	Region    string                     `yaml:"region"`
	Tags      []string                   `yaml:"tags"`
	VPC       VPC                        `yaml:"vpc"`
	SSH       SSH                        `yaml:"ssh"`
	Instances map[string]Instance        `yaml:"instances"`
	Firewalls map[string]FirewallProfile `yaml:"firewalls"`
	Deploy    Deploy                     `yaml:"deploy"`
	Digest    string                     `yaml:"-"`
}

type VPC struct {
	Label  string `yaml:"label"`
	CIDR   string `yaml:"cidr"`
	Subnet struct {
		Label string `yaml:"label"`
		CIDR  string `yaml:"cidr"`
	} `yaml:"subnet"`
}

type SSH struct {
	User               string   `yaml:"user"`
	PublicKeyPath      string   `yaml:"public_key_path"`
	AllowedSourceCIDRs []string `yaml:"allowed_source_cidrs"`
	BastionHost        string   `yaml:"bastion_host"`
}

type Instance struct {
	Label           string   `yaml:"label"`
	Type            string   `yaml:"type"`
	Image           string   `yaml:"image"`
	Networking      string   `yaml:"networking"`
	PrivateIP       string   `yaml:"private_ip"`
	FirewallProfile string   `yaml:"firewall_profile"`
	Roles           []string `yaml:"roles"`
}

type FirewallProfile struct {
	PublicTCP                []string `yaml:"public_tcp"`
	PrivateTCP               []string `yaml:"private_tcp"`
	PrivateEgressTCP         []string `yaml:"private_egress_tcp"`
	OptionalPrivateEgressTCP []string `yaml:"optional_private_egress_tcp"`
}

type Deploy struct {
	Release       string   `yaml:"release"`
	Domain        string   `yaml:"domain"`
	PublicBaseURL string   `yaml:"public_base_url"`
	APIPort       int      `yaml:"api_port"`
	DatabaseName  string   `yaml:"database_name"`
	DatabaseRole  string   `yaml:"database_role"`
	InstallPrefix string   `yaml:"install_prefix"`
	EtcDir        string   `yaml:"etc_dir"`
	StateDir      string   `yaml:"state_dir"`
	BackupMarker  string   `yaml:"backup_marker"`
	RestartUnits  []string `yaml:"restart_units"`
	WorkerUnits   []string `yaml:"worker_units"`
}

func Load(path string) (Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return Manifest{}, err
	}
	sum := sha256.Sum256(b)
	m.Digest = fmt.Sprintf("%x", sum[:])
	return m, m.Validate()
}

func (m Manifest) Validate() error {
	if m.Stack == "" || m.Region == "" {
		return fmt.Errorf("stack and region are required")
	}
	if _, _, err := net.ParseCIDR(m.VPC.CIDR); err != nil {
		return fmt.Errorf("invalid vpc cidr: %w", err)
	}
	_, subnet, err := net.ParseCIDR(m.VPC.Subnet.CIDR)
	if err != nil {
		return fmt.Errorf("invalid subnet cidr: %w", err)
	}
	if m.SSH.User == "" || m.SSH.PublicKeyPath == "" || m.SSH.BastionHost == "" {
		return fmt.Errorf("ssh user, public_key_path, and bastion_host are required")
	}
	for _, cidr := range m.SSH.AllowedSourceCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("invalid allowed ssh cidr %q: %w", cidr, err)
		}
	}
	for _, role := range []string{"edge", "infra", "account-manager"} {
		inst, ok := m.Instances[role]
		if !ok {
			return fmt.Errorf("missing instance %s", role)
		}
		if inst.Label == "" || inst.Networking == "" || inst.PrivateIP == "" || inst.FirewallProfile == "" {
			return fmt.Errorf("instance %s missing required fields", role)
		}
		if _, ok := m.Firewalls[inst.FirewallProfile]; !ok {
			return fmt.Errorf("instance %s references missing firewall profile %s", role, inst.FirewallProfile)
		}
		ip := net.ParseIP(inst.PrivateIP)
		if ip == nil || !subnet.Contains(ip) {
			return fmt.Errorf("instance %s private_ip %q is outside subnet %s", role, inst.PrivateIP, subnet.String())
		}
	}
	acct := m.Instances["account-manager"]
	if acct.Networking != "vpc_only" {
		return fmt.Errorf("account-manager networking must be vpc_only")
	}
	if acct.PrivateIP != "10.42.1.20" {
		return fmt.Errorf("account-manager private_ip must be 10.42.1.20")
	}
	if acct.Type == "" || acct.Image == "" {
		return fmt.Errorf("account-manager type and image are required")
	}
	if m.Deploy.Release == "" || strings.EqualFold(m.Deploy.Release, "latest") {
		return fmt.Errorf("explicit release is required; latest is not allowed")
	}
	if m.Deploy.Domain == "" || m.Deploy.PublicBaseURL == "" || m.Deploy.APIPort != 18081 {
		return fmt.Errorf("domain, public_base_url, and api_port 18081 are required")
	}
	if m.Deploy.DatabaseName != "rtk_account_manager" || m.Deploy.DatabaseRole != "rtk_account_manager" {
		return fmt.Errorf("account-manager database and role must be isolated as rtk_account_manager")
	}
	if m.Deploy.InstallPrefix == "" || m.Deploy.EtcDir == "" || m.Deploy.StateDir == "" || m.Deploy.BackupMarker == "" {
		return fmt.Errorf("install_prefix, etc_dir, state_dir, and backup_marker are required")
	}
	return nil
}

func (m Manifest) AccountManager() Instance {
	return m.Instances["account-manager"]
}

func (m Manifest) Edge() Instance {
	return m.Instances["edge"]
}

func (m Manifest) Infra() Instance {
	return m.Instances["infra"]
}

func (m Manifest) WorkersDisabledByDefault() bool {
	return len(m.Deploy.WorkerUnits) > 0
}
