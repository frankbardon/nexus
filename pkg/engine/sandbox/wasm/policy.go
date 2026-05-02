package wasm

import (
	"fmt"

	"github.com/frankbardon/nexus/pkg/engine/sandbox/wasm/host"
)

// parsePolicy lifts the network, fs_mounts, exec_allowed, and env config
// blocks into a host.Policy used by the bridge gates. Empty config means
// "deny all" — bridge calls will fail with capability denial unless the
// caller opts in explicitly.
func parsePolicy(cfg map[string]any) (host.Policy, error) {
	var p host.Policy

	if rawNet, ok := cfg["net"].(map[string]any); ok {
		if hosts, ok := rawNet["allow_hosts"].([]any); ok {
			for _, h := range hosts {
				if s, ok := h.(string); ok && s != "" {
					p.NetAllowHosts = append(p.NetAllowHosts, s)
				}
			}
		}
	}

	if rawMounts, ok := cfg["fs_mounts"].([]any); ok {
		for _, m := range rawMounts {
			block, ok := m.(map[string]any)
			if !ok {
				continue
			}
			mount := host.FSMount{
				Mode: "ro",
			}
			if s, ok := block["host"].(string); ok {
				expanded, err := expandHome(s)
				if err != nil {
					return host.Policy{}, fmt.Errorf("fs_mounts host %q: %w", s, err)
				}
				mount.Host = expanded
			}
			if s, ok := block["guest"].(string); ok {
				mount.Guest = s
			}
			if s, ok := block["mode"].(string); ok && s != "" {
				if s != "ro" && s != "rw" {
					return host.Policy{}, fmt.Errorf("fs_mounts mode %q: want ro or rw", s)
				}
				mount.Mode = s
			}
			if mount.Host == "" || mount.Guest == "" {
				return host.Policy{}, fmt.Errorf("fs_mounts entry needs both host and guest")
			}
			p.FSMounts = append(p.FSMounts, mount)
		}
	}

	if raw, ok := cfg["exec_allowed"].([]any); ok {
		for _, e := range raw {
			if s, ok := e.(string); ok && s != "" {
				p.ExecAllowed = append(p.ExecAllowed, s)
			}
		}
	}

	if raw, ok := cfg["env"].(map[string]any); ok {
		p.EnvVars = make(map[string]string, len(raw))
		for k, v := range raw {
			if s, ok := v.(string); ok {
				p.EnvVars[k] = s
			}
		}
	}

	return p, nil
}
