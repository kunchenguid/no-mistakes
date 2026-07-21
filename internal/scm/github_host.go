package scm

// GitHubHostConfigured reports whether gh has an authenticated entry for host.
func GitHubHostConfigured(host string) bool {
	return ghKnowsHost(ExtractHost(host))
}

// GitHubCanonicalWebHost returns the unique authenticated gh web authority for
// a hostname, preserving a configured HTTPS port.
func GitHubCanonicalWebHost(host string) (string, bool) {
	return ghCanonicalWebHost(ExtractHost(host))
}

func GitHubCanonicalWebAuthority(authority string) (string, bool) {
	return ghCanonicalWebAuthority(authority)
}

// GitHubWebHostConfigured reports whether gh has an exact authenticated web
// authority entry.
func GitHubWebHostConfigured(authority string) bool {
	return ghWebHostConfigured(authority)
}
