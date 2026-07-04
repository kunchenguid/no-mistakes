package steps

import "testing"

func TestIsTestFile(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		path string
		want bool
	}{
		// Go
		{"go test", "internal/pipeline/steps/foo_test.go", true},
		{"go source", "internal/pipeline/steps/foo.go", false},
		// Rust
		{"rust test", "src/foo_test.rs", true},
		// Python
		{"python test_ prefix", "pkg/test_foo.py", true},
		{"python _test suffix", "pkg/foo_test.py", true},
		{"python source", "pkg/foo.py", false},
		// Ruby
		{"ruby test_ prefix", "test/test_foo.rb", true},
		// Java
		{"java Test", "src/FooTest.java", true},
		{"java Tests", "src/FooTests.java", true},
		// JS/TS
		{"ts test", "src/foo.test.ts", true},
		{"tsx spec", "src/foo.spec.tsx", true},
		// Swift / XCTest
		{"swift Tests suffix", "MyAppTests/LoginTests.swift", true},
		{"swift Test suffix", "MyAppTests/LoginTest.swift", true},
		{"swift under Tests dir (SPM)", "Tests/AppTests/NetworkClient.swift", true},
		{"swift under nested Tests dir", "Sources/App/Tests/HelperSpec.swift", true},
		{"swift under top-level Tests dir", "Tests/Helper.swift", true},
		{"swift production source", "Sources/App/LoginViewController.swift", false},
		{"swift production source at root", "AppDelegate.swift", false},
		// Non-test noise
		{"empty", "", false},
		{"readme", "README.md", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTestFile(tc.path); got != tc.want {
				t.Fatalf("isTestFile(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}
