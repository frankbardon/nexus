package host

import (
	"net/url"
	"path/filepath"
	"strings"
)

// netHostAllowed enforces the configured allow_hosts list. Empty list means
// "deny all" — explicit allowlisting only.
func (b *Bridge) netHostAllowed(rawURL string) bool {
	if len(b.policy.NetAllowHosts) == 0 {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	for _, allowed := range b.policy.NetAllowHosts {
		if host == allowed {
			return true
		}
	}
	return false
}

// fsReadAllowed reports whether guestPath falls under any configured mount.
// Read access is granted by any mount; write requires Mode == "rw".
func (b *Bridge) fsReadAllowed(guestPath string) bool {
	clean := filepath.Clean(guestPath)
	for _, m := range b.policy.FSMounts {
		guest := filepath.Clean(m.Guest)
		if clean == guest || strings.HasPrefix(clean, guest+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// fsWriteAllowed reports whether guestPath falls under a writable mount.
func (b *Bridge) fsWriteAllowed(guestPath string) bool {
	clean := filepath.Clean(guestPath)
	for _, m := range b.policy.FSMounts {
		guest := filepath.Clean(m.Guest)
		if clean == guest || strings.HasPrefix(clean, guest+string(filepath.Separator)) {
			if m.Mode == "rw" {
				return true
			}
			return false
		}
	}
	return false
}

// execAllowed reports whether name is in the allowed-commands list.
func (b *Bridge) execAllowed(name string) bool {
	for _, allowed := range b.policy.ExecAllowed {
		if name == allowed {
			return true
		}
	}
	return false
}

// MountForGuest returns the host-side directory backing a guest path along
// with the relative offset of guestPath within that mount, or "" / "" if no
// mount covers it. Used by the default capabilities adapter to translate a
// guest path into a real host filesystem path.
func MountForGuest(mounts []FSMount, guestPath string) (host, rel string) {
	clean := filepath.Clean(guestPath)
	for _, m := range mounts {
		guest := filepath.Clean(m.Guest)
		if clean == guest {
			return filepath.Clean(m.Host), ""
		}
		if strings.HasPrefix(clean, guest+string(filepath.Separator)) {
			return filepath.Clean(m.Host), strings.TrimPrefix(clean, guest+string(filepath.Separator))
		}
	}
	return "", ""
}
