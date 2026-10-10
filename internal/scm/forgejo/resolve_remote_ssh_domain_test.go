package forgejo

import (
	"strings"
	"testing"
)

const (
	sshDomainBase   = "https://forge.example:3443/git"
	sshDomainHost   = "ssh.forge.example"
	sshDomainRemote = "git@ssh.forge.example:git/octo/widgets.git"
)

func TestResolveRemoteWithSSHDomainAcceptsDeclaredDomain(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		remote   string
		resolved string
		domain   string
	}{
		{
			name:   "scp remote on the declared domain",
			remote: sshDomainRemote,
			domain: sshDomainHost,
		},
		{
			name:   "ssh URL with a clone port",
			remote: "ssh://git@ssh.forge.example:2222/git/octo/widgets.git",
			domain: sshDomainHost,
		},
		{
			name:     "alias resolved to the declared domain",
			remote:   "git@forgejo-work:git/octo/widgets.git",
			resolved: sshDomainHost,
			domain:   sshDomainHost,
		},
		{
			name:   "declared domain carries a port and mixed case",
			remote: sshDomainRemote,
			domain: "SSH.Forge.Example:2222",
		},
		{
			name:   "web host still accepted while a domain is declared",
			remote: "git@forge.example:git/octo/widgets.git",
			domain: sshDomainHost,
		},
		{
			name:   "pull request URL path",
			remote: "ssh://git@ssh.forge.example:2222/git/octo/widgets/pulls/42",
			domain: sshDomainHost,
		},
		{
			name:   "IPv4 clone host",
			remote: "git@10.0.0.5:git/octo/widgets.git",
			domain: "10.0.0.5",
		},
		{
			// The remote URL yields a bare address while scm.ResolveHost yields a
			// bracketed one, so both spellings have to reach the declared address.
			name:   "bracketed IPv6 clone host",
			remote: "ssh://git@[2001:db8::1]:2222/git/octo/widgets.git",
			domain: "[2001:db8::1]",
		},
		{
			name:     "bracketed IPv6 resolved host",
			remote:   "git@forgejo-work:git/octo/widgets.git",
			resolved: "[2001:db8::1]",
			domain:   "[2001:db8::1]",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			base, repo, err := ResolveRemoteWithSSHDomain(tt.remote, sshDomainBase, tt.resolved, tt.domain)
			if err != nil {
				t.Fatalf("ResolveRemoteWithSSHDomain() error = %v", err)
			}
			if base != sshDomainBase || repo != testRepo {
				t.Fatalf("ResolveRemoteWithSSHDomain() = (%q, %q), want (%q, %q)", base, repo, sshDomainBase, testRepo)
			}
		})
	}
}

func TestResolveRemoteWithSSHDomainRejectsOtherHosts(t *testing.T) {
	t.Parallel()
	_, _, err := ResolveRemoteWithSSHDomain("git@other.example:git/octo/widgets.git", sshDomainBase, "", sshDomainHost)
	if err == nil {
		t.Fatal("ResolveRemoteWithSSHDomain() error = nil, want a host mismatch")
	}
	// The rejection has to name the resolved host and every accepted host so the
	// operator can see which setting to correct.
	for _, want := range []string{`"other.example"`, `"forge.example"`, `"ssh.forge.example"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("ResolveRemoteWithSSHDomain() error = %v, want it to name %s", err, want)
		}
	}
}

func TestResolveRemoteWithSSHDomainIgnoresUnusableDomains(t *testing.T) {
	t.Parallel()
	// A malformed setting can never widen the accepted hosts, but the operator
	// has to learn that the value was rejected rather than merely unmatched.
	for _, domain := range []string{
		"https://ssh.forge.example",
		"ssh.forge.example/git",
		"git@ssh.forge.example",
		"ssh.forge.example:port",
		"*.forge.example",
		"ssh.forge.example.",
		"::1",
	} {
		_, _, err := ResolveRemoteWithSSHDomain(sshDomainRemote, sshDomainBase, "", domain)
		if err == nil {
			t.Fatalf("ResolveRemoteWithSSHDomain() with domain %q error = nil, want a host mismatch", domain)
		}
		if !strings.Contains(err.Error(), `"ssh.forge.example" does not match`) {
			t.Fatalf("ResolveRemoteWithSSHDomain() with domain %q error = %v, want the resolved host named", domain, err)
		}
		if !strings.Contains(err.Error(), "FORGEJO_SSH_DOMAIN was ignored") {
			t.Fatalf("ResolveRemoteWithSSHDomain() with domain %q error = %v, want the ignored setting reported", domain, err)
		}
		if strings.Contains(err.Error(), "or declared FORGEJO_SSH_DOMAIN") {
			t.Fatalf("ResolveRemoteWithSSHDomain() with domain %q error = %v, want it offered as no accepted host", domain, err)
		}
	}

	// An unset domain leaves the original message untouched: nothing was ignored.
	for _, domain := range []string{"", "   "} {
		_, _, err := ResolveRemoteWithSSHDomain(sshDomainRemote, sshDomainBase, "", domain)
		if err == nil {
			t.Fatalf("ResolveRemoteWithSSHDomain() with domain %q error = nil, want a host mismatch", domain)
		}
		if !strings.Contains(err.Error(), `"ssh.forge.example" does not match`) {
			t.Fatalf("ResolveRemoteWithSSHDomain() with domain %q error = %v, want the resolved host named", domain, err)
		}
		if strings.Contains(err.Error(), "FORGEJO_SSH_DOMAIN") {
			t.Fatalf("ResolveRemoteWithSSHDomain() with domain %q error = %v, want no mention of an SSH domain", domain, err)
		}
	}
}

func TestResolveRemoteWithSSHDomainKeepsRejectedDomainsOutOfTheError(t *testing.T) {
	t.Parallel()
	// buildHost turns this error into a step skip reason that is logged and
	// persisted, so a credential pasted into FORGEJO_SSH_DOMAIN must not travel
	// with it.
	const credential = "PLACEHOLDER-CREDENTIAL"
	for _, domain := range []string{
		"git:" + credential + "@ssh.forge.example",
		"ssh://git:" + credential + "@ssh.forge.example:2222",
		"ssh.forge.example?token=" + credential,
	} {
		_, _, err := ResolveRemoteWithSSHDomain(sshDomainRemote, sshDomainBase, "", domain)
		if err == nil {
			t.Fatalf("ResolveRemoteWithSSHDomain() with domain %q error = nil, want a host mismatch", domain)
		}
		if strings.Contains(err.Error(), credential) {
			t.Fatalf("ResolveRemoteWithSSHDomain() error = %v, want no part of the rejected domain", err)
		}
		if !strings.Contains(err.Error(), "FORGEJO_SSH_DOMAIN was ignored") {
			t.Fatalf("ResolveRemoteWithSSHDomain() error = %v, want the ignored setting reported", err)
		}
	}

	// An accepted domain is safe to name: it is a validated bare hostname or IP
	// address by the time it reaches the message.
	_, _, err := ResolveRemoteWithSSHDomain("git@other.example:git/octo/widgets.git", sshDomainBase, "", sshDomainHost)
	if err == nil || !strings.Contains(err.Error(), sshDomainHost) {
		t.Fatalf("ResolveRemoteWithSSHDomain() error = %v, want the accepted domain named", err)
	}
}

func TestResolveRemoteWithSSHDomainLeavesHTTPSRemotesUnchanged(t *testing.T) {
	t.Parallel()
	base, repo, err := ResolveRemoteWithSSHDomain(sshDomainBase+"/octo/widgets.git", sshDomainBase, "", sshDomainHost)
	if err != nil {
		t.Fatalf("ResolveRemoteWithSSHDomain() error = %v", err)
	}
	if base != sshDomainBase || repo != testRepo {
		t.Fatalf("ResolveRemoteWithSSHDomain() = (%q, %q), want (%q, %q)", base, repo, sshDomainBase, testRepo)
	}

	// An HTTPS remote served from the declared SSH host is still a mismatch: the
	// SSH domain names a clone host, never a second web host.
	if _, _, err := ResolveRemoteWithSSHDomain("https://ssh.forge.example:3443/git/octo/widgets.git", sshDomainBase, "", sshDomainHost); err == nil {
		t.Fatal("ResolveRemoteWithSSHDomain() error = nil, want a host mismatch for an HTTPS remote")
	}
}

func TestResolveRemoteMatchesResolveRemoteWithoutSSHDomain(t *testing.T) {
	t.Parallel()
	wantBase, wantRepo, wantErr := ResolveRemoteWithSSHDomain(sshDomainRemote, sshDomainBase, "", "")
	if wantErr == nil {
		t.Fatal("ResolveRemoteWithSSHDomain() error = nil, want a host mismatch without a declared domain")
	}
	base, repo, err := ResolveRemote(sshDomainRemote, sshDomainBase, "")
	if base != wantBase || repo != wantRepo || err == nil || err.Error() != wantErr.Error() {
		t.Fatalf("ResolveRemote() = (%q, %q, %v), want (%q, %q, %v)", base, repo, err, wantBase, wantRepo, wantErr)
	}
}
