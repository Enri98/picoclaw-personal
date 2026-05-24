package vertex

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// cloudPlatformScope is the OAuth2 scope required for Vertex AI API calls.
const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// newTokenSource returns an oauth2.TokenSource that auto-refreshes using
// Application Default Credentials. If credentialsPath is non-empty, that file
// is used; otherwise the standard ADC lookup order applies
// (GOOGLE_APPLICATION_CREDENTIALS → gcloud user credentials → metadata server).
func newTokenSource(ctx context.Context, credentialsPath string) (oauth2.TokenSource, error) {
	var creds *google.Credentials
	var err error

	if credentialsPath != "" {
		data, readErr := os.ReadFile(credentialsPath)
		if readErr != nil {
			return nil, fmt.Errorf("vertex: reading credentials file %q: %w", credentialsPath, readErr)
		}
		creds, err = google.CredentialsFromJSON(ctx, data, cloudPlatformScope)
	} else {
		creds, err = google.FindDefaultCredentials(ctx, cloudPlatformScope)
	}
	if err != nil {
		return nil, fmt.Errorf("vertex: loading credentials: %w", err)
	}

	return oauth2.ReuseTokenSource(nil, creds.TokenSource), nil
}

// newOAuthTransport wraps base with an oauth2.Transport so that every request
// gets a valid Bearer token injected. This lets us preserve a proxy-aware
// base transport (from common.NewHTTPClient) while still handling auth.
func newOAuthTransport(ts oauth2.TokenSource, base http.RoundTripper) http.RoundTripper {
	return &oauth2.Transport{
		Source: ts,
		Base:   base,
	}
}
