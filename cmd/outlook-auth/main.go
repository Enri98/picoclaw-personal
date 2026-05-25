// outlook-auth obtains an Outlook refresh token via the Microsoft device-code
// flow. Run it once on a laptop or any machine with a browser available.
//
// Usage:
//
//	outlook-auth
//
// Required environment variable:
//
//	OUTLOOK_OAUTH_CLIENT_ID
//
// On success it prints to stdout:
//
//	OUTLOOK_REFRESH_TOKEN=<token>
//
// and writes a short confirmation to stderr. Exit code 0 on success,
// non-zero on any error. Exit code 2 indicates a configuration error.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	tenantBase     = "https://login.microsoftonline.com/consumers/oauth2/v2.0"
	devicecodeURL  = tenantBase + "/devicecode"
	tokenURL       = tenantBase + "/token"
	scopeParam     = "Mail.Read offline_access"
	deviceCodeGT   = "urn:ietf:params:oauth:grant-type:device_code"
)

func main() {
	clientID := os.Getenv("OUTLOOK_OAUTH_CLIENT_ID")
	if clientID == "" {
		fmt.Fprintln(os.Stderr, "error: OUTLOOK_OAUTH_CLIENT_ID environment variable is not set")
		os.Exit(2)
	}

	if err := run(clientID); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

type deviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
	Message         string `json:"message"`
}

type tokenResponse struct {
	RefreshToken string `json:"refresh_token"`
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func run(clientID string) error {
	// Step 1: request device code.
	dcResp, err := requestDeviceCode(clientID)
	if err != nil {
		return fmt.Errorf("device code request failed: %w", err)
	}

	interval := dcResp.Interval
	if interval <= 0 {
		interval = 5
	}
	expiresIn := dcResp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 900
	}

	// Print instructions.
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintf(os.Stderr, "Open %s in your browser and enter code: %s\n",
		dcResp.VerificationURI, dcResp.UserCode)
	fmt.Fprintf(os.Stderr, "Code expires in %d minutes.\n", expiresIn/60)
	fmt.Fprintln(os.Stderr, "")

	// Step 2: poll the token endpoint.
	deadline := time.Now().Add(time.Duration(expiresIn) * time.Second)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for device authorization (%d seconds elapsed)", expiresIn)
		}

		time.Sleep(time.Duration(interval) * time.Second)

		tok, done, err := pollToken(clientID, dcResp.DeviceCode)
		if err != nil {
			// Hard error.
			return fmt.Errorf("token poll failed: %w", err)
		}
		if done == "pending" {
			continue
		}
		if done == "slow_down" {
			interval += 5
			continue
		}
		if done == "ok" {
			fmt.Printf("OUTLOOK_REFRESH_TOKEN=%s\n", tok.RefreshToken)
			fmt.Fprintln(os.Stderr, "Success. Set OUTLOOK_REFRESH_TOKEN in your secrets.env file.")
			return nil
		}
		// Unexpected error code.
		return fmt.Errorf("authorization error %q: %s", done, tok.ErrorDesc)
	}
}

func requestDeviceCode(clientID string) (*deviceCodeResponse, error) {
	body := url.Values{}
	body.Set("client_id", clientID)
	body.Set("scope", scopeParam)

	resp, err := http.PostForm(devicecodeURL, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(raw))
	}

	var dc deviceCodeResponse
	if err := json.Unmarshal(raw, &dc); err != nil {
		return nil, fmt.Errorf("parsing device code response: %w", err)
	}
	if dc.DeviceCode == "" || dc.UserCode == "" {
		return nil, fmt.Errorf("malformed device code response: missing device_code or user_code")
	}
	return &dc, nil
}

// pollToken polls the token endpoint once. It returns:
//   - tok, "ok", nil on success
//   - nil, "pending", nil when authorization is still pending
//   - nil, "slow_down", nil when the server requests slower polling
//   - nil, "<error_code>", nil for other recognised OAuth errors
//   - nil, "", err for HTTP/network/parse failures
func pollToken(clientID, deviceCode string) (*tokenResponse, string, error) {
	body := url.Values{}
	body.Set("grant_type", deviceCodeGT)
	body.Set("client_id", clientID)
	body.Set("device_code", deviceCode)

	resp, err := http.PostForm(tokenURL, body)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("reading token response body: %w", err)
	}

	var tok tokenResponse
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, "", fmt.Errorf("parsing token response: %w", err)
	}

	if resp.StatusCode == http.StatusOK {
		if tok.RefreshToken == "" {
			return nil, "", fmt.Errorf("no refresh_token in successful response")
		}
		return &tok, "ok", nil
	}

	// 400-range errors carry an error field.
	switch strings.ToLower(tok.Error) {
	case "authorization_pending":
		return nil, "pending", nil
	case "slow_down":
		return nil, "slow_down", nil
	case "":
		return nil, "", fmt.Errorf("HTTP %d with no error field: %s", resp.StatusCode, string(raw))
	default:
		return &tok, tok.Error, nil
	}
}
