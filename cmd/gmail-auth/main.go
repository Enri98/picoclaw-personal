// gmail-auth is a cross-platform CLI helper for obtaining a Gmail OAuth2
// refresh token. Run it once per account on a laptop with a browser.
//
// Usage:
//
//	gmail-auth --account=<darra2|chiunque>
//
// Required environment variables:
//
//	GMAIL_OAUTH_CLIENT_ID
//	GMAIL_OAUTH_CLIENT_SECRET
//
// On success it prints to stdout:
//
//	GMAIL_REFRESH_TOKEN_<ACCOUNT>=<token>
//
// and writes a one-line confirmation to stderr. Exit code 0 on success,
// non-zero on any error.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	googleapi "google.golang.org/api/gmail/v1"
)

func main() {
	account := flag.String("account", "", "Account name: darra2 or chiunque (required)")
	flag.Parse()

	if *account == "" {
		fmt.Fprintln(os.Stderr, "error: --account is required (valid values: darra2, chiunque)")
		os.Exit(2)
	}
	*account = strings.ToLower(strings.TrimSpace(*account))
	if *account != "darra2" && *account != "chiunque" {
		fmt.Fprintf(os.Stderr, "error: unknown account %q; valid values: darra2, chiunque\n", *account)
		os.Exit(2)
	}

	clientID := os.Getenv("GMAIL_OAUTH_CLIENT_ID")
	if clientID == "" {
		fmt.Fprintln(os.Stderr, "error: GMAIL_OAUTH_CLIENT_ID environment variable is not set")
		os.Exit(2)
	}
	clientSecret := os.Getenv("GMAIL_OAUTH_CLIENT_SECRET")
	if clientSecret == "" {
		fmt.Fprintln(os.Stderr, "error: GMAIL_OAUTH_CLIENT_SECRET environment variable is not set")
		os.Exit(2)
	}

	if err := run(clientID, clientSecret, *account); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(clientID, clientSecret, account string) error {
	// Start a local HTTP server on a random port.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("failed to start local server: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURL := fmt.Sprintf("http://127.0.0.1:%d/", port)

	cfg := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       []string{googleapi.GmailReadonlyScope},
		Endpoint:     google.Endpoint,
		RedirectURL:  redirectURL,
	}

	// Build the auth URL with offline access and forced consent so we always
	// get a refresh token. The state parameter is a random nonce checked in
	// the callback handler to defend against a malicious page in the same
	// browser session forging a callback with a stolen code.
	stateNonce, err := randomState()
	if err != nil {
		return fmt.Errorf("failed to generate state nonce: %w", err)
	}
	authURL := cfg.AuthCodeURL(
		stateNonce,
		oauth2.AccessTypeOffline,
		oauth2.ApprovalForce,
	)

	fmt.Fprintln(os.Stderr, "Opening browser for Gmail OAuth consent...")
	if err := openBrowser(authURL); err != nil {
		fmt.Fprintf(os.Stderr, "Could not open browser automatically. Open this URL manually:\n%s\n", authURL)
	}

	// Wait for the OAuth callback.
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		gotState := r.URL.Query().Get("state")
		if gotState != stateNonce {
			http.Error(w, "OAuth state mismatch", http.StatusBadRequest)
			errCh <- fmt.Errorf("OAuth state mismatch: expected %q, got %q", stateNonce, gotState)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			errMsg := r.URL.Query().Get("error")
			if errMsg == "" {
				errMsg = "no code received"
			}
			http.Error(w, "OAuth failed: "+errMsg, http.StatusBadRequest)
			errCh <- fmt.Errorf("OAuth callback error: %s", errMsg)
			return
		}
		fmt.Fprintln(w, "Authorization successful. You may close this tab.")
		codeCh <- code
	})

	srv := &http.Server{Handler: mux}
	go func() {
		if serveErr := srv.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- fmt.Errorf("HTTP server error: %w", serveErr)
		}
	}()
	defer func() { _ = srv.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return fmt.Errorf("timed out waiting for OAuth callback (5 minutes)")
	}

	// Exchange code for token.
	tok, err := cfg.Exchange(ctx, code)
	if err != nil {
		return fmt.Errorf("token exchange failed: %w", err)
	}
	if tok.RefreshToken == "" {
		return fmt.Errorf("no refresh token in response; ensure ApprovalForce is set and the app has offline access")
	}

	// Output the env var assignment to stdout for easy capture.
	envVarName := fmt.Sprintf("GMAIL_REFRESH_TOKEN_%s", strings.ToUpper(account))
	fmt.Printf("%s=%s\n", envVarName, tok.RefreshToken)
	fmt.Fprintf(os.Stderr, "Success. Set %s in your secrets.env file.\n", envVarName)

	return nil
}

// randomState returns a 32-character hex nonce sourced from crypto/rand.
func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// openBrowser attempts to open url in the user's default browser. It tries
// platform-specific commands in order and returns an error only if all fail.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
