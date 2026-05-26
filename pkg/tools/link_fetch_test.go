package tools

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// validateFetchURL tests
// ---------------------------------------------------------------------------

func TestValidateFetchURL(t *testing.T) {
	t.Parallel()

	// resolvePublicHost picks an arbitrary public IP we can use in table-driven
	// tests without hardcoding an address that may change.
	publicHost := "example.com"

	cases := []struct {
		name    string
		rawURL  string
		wantErr bool
		errSnip string
	}{
		{
			name:    "public https ok",
			rawURL:  "https://" + publicHost + "/path",
			wantErr: false,
		},
		{
			name:    "public http ok",
			rawURL:  "http://" + publicHost + "/",
			wantErr: false,
		},
		{
			name:    "loopback ipv4 rejected",
			rawURL:  "http://127.0.0.1/",
			wantErr: true,
			errSnip: "loopback",
		},
		{
			name:    "loopback ipv6 rejected",
			rawURL:  "http://[::1]/",
			wantErr: true,
			errSnip: "loopback",
		},
		{
			name:    "private 10.x rejected",
			rawURL:  "http://10.0.0.1/",
			wantErr: true,
			errSnip: "private",
		},
		{
			name:    "private 192.168.x rejected",
			rawURL:  "http://192.168.1.1/",
			wantErr: true,
			errSnip: "private",
		},
		{
			name:    "private 172.16.x rejected",
			rawURL:  "http://172.16.0.1/",
			wantErr: true,
			errSnip: "private",
		},
		{
			name:    "link-local rejected",
			rawURL:  "http://169.254.1.1/",
			wantErr: true,
			errSnip: "link-local",
		},
		{
			name:    "unspecified rejected",
			rawURL:  "http://0.0.0.0/",
			wantErr: true,
			errSnip: "unspecified",
		},
		{
			name:    "file scheme rejected",
			rawURL:  "file:///etc/passwd",
			wantErr: true,
			errSnip: "scheme",
		},
		{
			name:    "ftp scheme rejected",
			rawURL:  "ftp://example.com/",
			wantErr: true,
			errSnip: "scheme",
		},
		{
			name:    "empty host rejected",
			rawURL:  "http:///path",
			wantErr: true,
			errSnip: "empty host",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parsed, parseErr := url.Parse(tc.rawURL)
			if parseErr != nil {
				if tc.wantErr {
					return // url.Parse error counts as expected failure
				}
				t.Fatalf("unexpected url.Parse error: %v", parseErr)
			}
			err := validateFetchURL(parsed)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.errSnip)
				}
				if tc.errSnip != "" && !strings.Contains(err.Error(), tc.errSnip) {
					t.Fatalf("expected error containing %q, got: %v", tc.errSnip, err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// link_fetch Execute tests (using httptest.Server + skipSSRFGuard)
// ---------------------------------------------------------------------------

func TestLinkFetchHTMLExtraction(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!DOCTYPE html><html><head><title>Test Page</title></head>`+
			`<body><article><p>Hello, world!</p></article></body></html>`)
	}))
	defer srv.Close()

	ts := NewLinkFetchToolset()
	ts.skipSSRFGuard = true

	result := ts.Tools()[0].Execute(context.Background(), map[string]any{"url": srv.URL})
	if result == nil {
		t.Fatal("got nil result")
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	body := result.ForLLM
	if !strings.Contains(body, "Hello, world!") {
		t.Fatalf("extracted text missing expected content; got: %s", body)
	}
}

func TestLinkFetchPlainText(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "plain text response")
	}))
	defer srv.Close()

	ts := NewLinkFetchToolset()
	ts.skipSSRFGuard = true

	result := ts.Tools()[0].Execute(context.Background(), map[string]any{"url": srv.URL})
	if result == nil {
		t.Fatal("got nil result")
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "plain text response") {
		t.Fatalf("expected plain text content; got: %s", result.ForLLM)
	}
}

func TestLinkFetchSizeCap(t *testing.T) {
	t.Parallel()

	// Serve a body exactly 1 byte over the hard cap to trigger truncation.
	bigBody := strings.Repeat("a", linkFetchBodyHardCap+1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, bigBody)
	}))
	defer srv.Close()

	ts := NewLinkFetchToolset()
	ts.skipSSRFGuard = true

	result := ts.Tools()[0].Execute(context.Background(), map[string]any{"url": srv.URL})
	if result == nil {
		t.Fatal("got nil result")
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, `"truncated":true`) {
		t.Fatalf("expected truncated:true; got: %s", result.ForLLM)
	}
}

func TestLinkFetchRedirectCap(t *testing.T) {
	t.Parallel()

	// Build a chain of servers that redirects beyond the max allowed.
	// We only need linkFetchMaxRedirects+1 hops to exceed the cap.
	var servers []*httptest.Server
	var urls []string

	// Create servers in reverse so each can reference the next.
	count := linkFetchMaxRedirects + 2
	for i := 0; i < count; i++ {
		servers = append(servers, nil) // placeholder
		urls = append(urls, "")
	}
	for i := count - 1; i >= 0; i-- {
		idx := i // capture
		var handler http.HandlerFunc
		if idx == count-1 {
			handler = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				fmt.Fprint(w, "final destination")
			}
		} else {
			handler = func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, urls[idx+1], http.StatusFound)
			}
		}
		srv := httptest.NewServer(handler)
		servers[idx] = srv
		urls[idx] = srv.URL
	}
	defer func() {
		for _, srv := range servers {
			srv.Close()
		}
	}()

	ts := NewLinkFetchToolset()
	ts.skipSSRFGuard = true

	// The fetch should succeed (stops at last response after redirect cap) or
	// return an error — either is acceptable; what must NOT happen is a panic
	// or an infinite loop.
	result := ts.Tools()[0].Execute(context.Background(), map[string]any{"url": urls[0]})
	if result == nil {
		t.Fatal("got nil result")
	}
	// Just verify we get a response (error or success) without hanging.
}

func TestLinkFetchMissingURL(t *testing.T) {
	t.Parallel()

	ts := NewLinkFetchToolset()
	result := ts.Tools()[0].Execute(context.Background(), map[string]any{})
	if result == nil {
		t.Fatal("got nil result")
	}
	if !result.IsError {
		t.Fatal("expected error for missing url")
	}
}

func TestLinkFetchSSRFBlocked(t *testing.T) {
	t.Parallel()

	ts := NewLinkFetchToolset()
	// skipSSRFGuard is false (default) — loopback must be rejected.
	result := ts.Tools()[0].Execute(context.Background(), map[string]any{"url": "http://127.0.0.1/"})
	if result == nil {
		t.Fatal("got nil result")
	}
	if !result.IsError {
		t.Fatal("expected error for loopback address")
	}
	if !strings.Contains(result.ForLLM, "loopback") {
		t.Fatalf("expected loopback error; got: %s", result.ForLLM)
	}
}

// ---------------------------------------------------------------------------
// checkIP unit tests
// ---------------------------------------------------------------------------

func TestCheckIP(t *testing.T) {
	t.Parallel()

	cases := []struct {
		ip      string
		wantErr bool
		errSnip string
	}{
		{"8.8.8.8", false, ""},
		{"1.1.1.1", false, ""},
		{"127.0.0.1", true, "loopback"},
		{"::1", true, "loopback"},
		{"10.0.0.1", true, "private"},
		{"192.168.0.1", true, "private"},
		{"172.16.0.1", true, "private"},
		{"169.254.1.2", true, "link-local"},
		{"0.0.0.0", true, "unspecified"},
		{"224.0.0.1", true, "multicast"},
		{"ff02::1", true, "multicast"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.ip, func(t *testing.T) {
			t.Parallel()
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("could not parse IP %q", tc.ip)
			}
			err := checkIP(ip)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %s, got nil", tc.ip)
				}
				if tc.errSnip != "" && !strings.Contains(err.Error(), tc.errSnip) {
					t.Fatalf("expected error containing %q for %s; got: %v", tc.errSnip, tc.ip, err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error for %s: %v", tc.ip, err)
				}
			}
		})
	}
}

