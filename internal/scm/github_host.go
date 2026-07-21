package scm

// GitHubHostConfigured reports whether gh has an authenticated entry for host.
func GitHubHostConfigured(host string) bool {
	return ghKnowsHost(ExtractHost(host))
}
