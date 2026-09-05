package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishFirewallAction_PublicOutputStaysGeneric(t *testing.T) {
	action := loadRequireAction(t, ".github/actions/publish-firewall")
	if action.Name == "" {
		t.Fatal("action name missing")
	}
	if action.Runs.Using != "composite" {
		t.Fatalf("using=%s", action.Runs.Using)
	}
	script, err := os.ReadFile(filepath.Join(".github/actions/publish-firewall", "check.sh"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(script)
	for _, leak := range []string{
		"cat \"$DIFF\"",
		"cat \"$TITLE\"",
		"cat \"$BODY\"",
		"cat \"$PRIVATE\"",
		"cat \"$COMMITS\"",
	} {
		if strings.Contains(body, leak) {
			t.Fatalf("check.sh prints a private file: %s", leak)
		}
	}
	if !strings.Contains(body, "publish-policy violation") {
		t.Fatal("check.sh must emit the generic public phrase")
	}
	if !strings.Contains(body, "no-mistakes firewall github-check") {
		t.Fatal("check.sh must invoke firewall github-check")
	}
}

func TestPublishFirewallWorkflow_SelfHostedNoPublicVIP(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/publish-firewall.yml")
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "runs-on: [self-hosted, no-mistakes]") {
		t.Fatal("publish-firewall must run on self-hosted no-mistakes runners")
	}
	if strings.Contains(content, "ubuntu-latest") {
		t.Fatal("publish-firewall must not use GitHub-hosted ubuntu-latest")
	}
	if !strings.Contains(content, "name: publish-policy") {
		t.Fatal("stable check name publish-policy missing")
	}
}

func TestPublishFirewallCluster_NoPublicVIP(t *testing.T) {
	portal, err := os.ReadFile("deploy/no-mistakes/portal.yaml")
	if err != nil {
		t.Fatal(err)
	}
	body := string(portal)
	if !strings.Contains(body, "type: ClusterIP") {
		t.Fatal("portal Service must be ClusterIP")
	}
	if strings.Contains(body, "kind: Ingress") || strings.Contains(body, "type: LoadBalancer") {
		t.Fatal("portal must not expose Ingress or LoadBalancer")
	}
	ns, err := os.ReadFile("deploy/no-mistakes/namespace.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ns), "name: no-mistakes") {
		t.Fatal("namespace must be no-mistakes")
	}
}
