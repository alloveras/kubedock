package support

import (
	"regexp"

	"github.com/miekg/dns"
)

// GetClientConfig reads the system resolver configuration from /etc/resolv.conf.
func GetClientConfig() *dns.ClientConfig {
	clientConfig, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	if err != nil {
		panic(err)
	}
	return clientConfig
}

// IsValidHostname reports whether hostname is a valid RFC-compliant DNS name
// (max 255 chars, labels of alphanumeric + hyphens separated by dots).
func IsValidHostname(hostname string) bool {
	if len(hostname) > 255 {
		return false
	}
	pattern := `^([a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9\-]*[a-zA-Z0-9])(\.([a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9\-]*[a-zA-Z0-9]))*$`
	matched, _ := regexp.MatchString(pattern, hostname)
	return matched
}
