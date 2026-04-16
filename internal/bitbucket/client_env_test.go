package bitbucket

import "testing"

func TestNewClientFromEnvReadsAmbientEnvironment(t *testing.T) {
	t.Setenv(envEmail, "ambient@example.com")
	t.Setenv(envToken, "ambient-token")
	t.Setenv(envAPIBaseURL, "https://ambient.example")

	client, err := NewClientFromEnv(nil)
	if err != nil {
		t.Fatalf("NewClientFromEnv(nil) error = %v", err)
	}
	if client.email != "ambient@example.com" {
		t.Fatalf("email = %q, want ambient@example.com", client.email)
	}
	if client.token != "ambient-token" {
		t.Fatalf("token = %q, want ambient-token", client.token)
	}
	if client.baseURL != "https://ambient.example" {
		t.Fatalf("baseURL = %q, want https://ambient.example", client.baseURL)
	}
}

func TestNewClientFromEnvPrefersExplicitEnvironment(t *testing.T) {
	t.Setenv(envEmail, "ambient@example.com")
	t.Setenv(envToken, "ambient-token")
	t.Setenv(envAPIBaseURL, "https://ambient.example")

	client, err := NewClientFromEnv([]string{
		envEmail + "=explicit@example.com",
		envToken + "=explicit-token",
		envAPIBaseURL + "=https://explicit.example",
	})
	if err != nil {
		t.Fatalf("NewClientFromEnv(explicit env) error = %v", err)
	}
	if client.email != "explicit@example.com" {
		t.Fatalf("email = %q, want explicit@example.com", client.email)
	}
	if client.token != "explicit-token" {
		t.Fatalf("token = %q, want explicit-token", client.token)
	}
	if client.baseURL != "https://explicit.example" {
		t.Fatalf("baseURL = %q, want https://explicit.example", client.baseURL)
	}
}
