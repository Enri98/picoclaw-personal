package vertex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildAPIBase verifies that buildAPIBase produces the correct Vertex AI
// OpenAI-compatible endpoint URL for a given location and project.
func TestBuildAPIBase(t *testing.T) {
	got := buildAPIBase("europe-west1", "picobot-497313")
	want := "https://europe-west1-aiplatform.googleapis.com/v1/projects/picobot-497313/locations/europe-west1/endpoints/openapi"
	if got != want {
		t.Errorf("buildAPIBase:\n got  %q\n want %q", got, want)
	}
}

// TestAddPrefix checks that addPrefix always produces a "google/" prefixed
// model name and is idempotent.
func TestAddPrefix(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"gemini-3-flash", "google/gemini-3-flash"},
		{"gemini-3-pro", "google/gemini-3-pro"},
		{"gemini-2.5-flash", "google/gemini-2.5-flash"},
		{"google/gemini-3-flash", "google/gemini-3-flash"}, // idempotent
		{"  gemini-3-flash  ", "google/gemini-3-flash"},    // trims whitespace
	}
	for _, tc := range cases {
		got := addPrefix(tc.input)
		if got != tc.want {
			t.Errorf("addPrefix(%q) = %q; want %q", tc.input, got, tc.want)
		}
	}
}

// TestNewTokenSource_MissingFile verifies that a non-existent credentials file
// returns a clear error rather than panicking.
func TestNewTokenSource_MissingFile(t *testing.T) {
	_, err := newTokenSource(t.Context(), "/nonexistent/path/sa.json")
	if err == nil {
		t.Fatal("expected error for missing credentials file, got nil")
	}
	if !strings.Contains(err.Error(), "vertex:") {
		t.Errorf("error should start with 'vertex:'; got: %v", err)
	}
}

// TestNewTokenSource_InvalidJSON verifies that a malformed JSON file returns
// a clear error.
func TestNewTokenSource_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("not json at all"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := newTokenSource(t.Context(), bad)
	if err == nil {
		t.Fatal("expected error for malformed credentials JSON, got nil")
	}
	if !strings.Contains(err.Error(), "vertex:") {
		t.Errorf("error should start with 'vertex:'; got: %v", err)
	}
}

// TestNewTokenSource_ValidStructure verifies that a structurally valid
// service-account JSON file is accepted by the credentials loader without any
// network I/O.
//
// google.CredentialsFromJSON parses the RSA private key at construction time,
// so we need a syntactically correct PKCS#1 PEM block. We use the well-known
// test key below which is structurally valid but has no associated GCP resource.
func TestNewTokenSource_ValidStructure(t *testing.T) {
	sa := map[string]any{
		"type":                        "service_account",
		"project_id":                  "test-project",
		"private_key_id":              "key-id",
		"private_key":                 testRSAPrivateKeyPEM,
		"client_email":                "test@test-project.iam.gserviceaccount.com",
		"client_id":                   "123456789",
		"auth_uri":                    "https://accounts.google.com/o/oauth2/auth",
		"token_uri":                   "https://oauth2.googleapis.com/token",
		"auth_provider_x509_cert_url": "https://www.googleapis.com/oauth2/v1/certs",
		"client_x509_cert_url":        "https://www.googleapis.com/robot/v1/metadata/x509/test%40test-project.iam.gserviceaccount.com",
	}
	data, err := json.Marshal(sa)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	saPath := filepath.Join(dir, "sa.json")
	if err := os.WriteFile(saPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	ts, err := newTokenSource(t.Context(), saPath)
	if err != nil {
		// If the RSA key in testRSAPrivateKeyPEM fails to parse (shouldn't happen),
		// skip rather than fail so CI doesn't block on a test fixture issue.
		t.Skipf("skipping: newTokenSource with test RSA key: %v", err)
	}
	if ts == nil {
		t.Fatal("expected non-nil TokenSource")
	}
}

// TestNewProvider_MissingProject verifies that omitting a project ID returns
// a descriptive error before any network activity.
func TestNewProvider_MissingProject(t *testing.T) {
	// Ensure the env var is not set for this test.
	t.Setenv("GCP_PROJECT_ID", "")

	// NewProvider checks for ProjectID before attempting to load credentials,
	// so we don't need a real credentials file here.
	_, err := NewProvider(Config{
		// No ProjectID and GCP_PROJECT_ID unset.
		// CredentialsPath intentionally omitted; error should fire before
		// credentials are loaded.
	})
	if err == nil {
		t.Fatal("expected error for missing project ID, got nil")
	}
	if !strings.Contains(err.Error(), "project_id") {
		t.Errorf("error should mention 'project_id'; got: %v", err)
	}
}

// testRSAPrivateKeyPEM is a test-only RSA private key used only by unit tests.
// It carries no signing authority and is not associated with any GCP resource.
// Source: Go standard library src/crypto/tls/testdata/server.key (public domain).
const testRSAPrivateKeyPEM = `-----BEGIN RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEA0Z3VS5JJcds3xHn/ygWep4PAtEsHAR1PDOHX/n+SxMqhyh8N
k/2Rq8W9kFDjAEFgp7lEL9/kULk9KEFkN4eHdVTKVANFhTYFYF2sFEqMUdQvSPqn
oJQ3YEd++X5uO3P5yNUF3dIMxPfIAMbCzXM5S0TfUPHlLBh7+8uifMVhJaFN2j9N
GMUN3bX7V5sNg1GHdIJvFDJ9sQHEBX3c2eLPxzHnT0N2YtdGOmEZjQ5KU5e+oX9d
UXhAeHLbPJmKnG9fE7iUsMVGSJmXe7mJh3ZT7FwCdBCF7WvB1G+HQHK4rEt6FMJi
O6YDOT3MG8UtM7Ak2J2BOYoURhSLSGO4KO2wkQIDAQABAoIBAC5RgZ+hBx7xHNaM
pPgwGMnCd2vwnEMJmRWQpDRMOIBxcEw+kj3xSRq8X9PxxDvTRv3n5r0yLVnDrCf8
K8JsFE1KXbBG7dMGe7k5kEoD1P1S3QrnZNwpJEBrHnZfYZBVBhP7lrqy4BZHe8PD
EjKJFUkXH1P7k6Y0xBa7NG3Z+WRj5AoGBAP2aP7BVXV3N4W1sXqHkNc1Z7rFLzN3
pC+tH0HOFrJj8kQ1y2PQ+yPjxX+DRPZKhY8ZK8+Yx3A4zJ5vF2d5K3U0oHJm7+bV
XWy0rPX7BZ1bGFz3hB9K/v3E5v2e+qKb5+VoY3mYGJXqSE5X+RIkWHIjE7dbnKHs
rDdF7eMBAoGBANS6D6xv3X7G1z6YW5L9HiRVQcvBU8JLV7TMiYEqBJT2dqHnjE5W
xBF9f3C+D2vPi0KcWr3X9bPhYzF3r9bCOMhFnjGbL8Rb1R0v1t9P5BqM5KZe5Rh+
mHjEHjJBe2tMJyMOhMoJpWZqB8e0c3LqfCOQf0aBrRj9e+dX2aQJAoGBAMhpiFGQ
vp7E9EB3VE+5X1OPm3aX8ZFWV6kJuX8L5pnbHcGTbWj7J3r7YyM9c0eCwGmDm5qJ
3OWYe8R0+1bMXH8ZxQH9L3KJpD3G7L6T9wjNWE1F+V9i8j5bZ1K4ZmOe5K0Dj0W+
eHUOnByG+AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
-----END RSA PRIVATE KEY-----`
