package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// sentinel-cli-browser-login (docs/plans/CLI_BROWSER_LOGIN.md): `sentinel login` without --key-stdin runs an RFC 8628
// device authorization flow against the dashboard:
//
//	POST /api/cli/device        -> device_code (kept in memory only), user_code, verification URI
//	(the person approves the user_code at <url>/cli/approve, signed in to the dashboard)
//	POST /api/cli/device/token  -> polled every `interval` seconds until the key arrives (once) or the request ends
//
// It needs no localhost callback, so it works over SSH and on headless machines: the URL can be opened on any device.
// The key it receives is a PERSONAL key -- it acts as the approving person and never exceeds their dashboard access.

// deviceStart is POST /api/cli/device's response.
type deviceStart struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// deviceToken is POST /api/cli/device/token's success response. Key is the secret: it is saved to the config file
// (0600) and never printed.
type deviceToken struct {
	Key            string `json:"key"`
	KeyPrefix      string `json:"key_prefix"`
	KeyID          string `json:"key_id"`
	OrganizationID string `json:"organization_id"`
	OrgSlug        string `json:"org_slug"`
	URL            string `json:"url"`
	CLIProfile     string `json:"cli_profile"`
	ExpiresAt      string `json:"expires_at"`
	Owner          struct {
		Name  *string `json:"name"`
		Email string  `json:"email"`
		Role  string  `json:"role"`
	} `json:"owner"`
}

// deviceError is the RFC 6749 §5.2 error body both device routes answer with.
type deviceError struct {
	Error       string `json:"error"`
	Description string `json:"error_description"`
	Interval    int    `json:"interval"`
	RetryAfter  int    `json:"retry_after"`
}

// openBrowser tries to open url in the user's browser and never blocks or fails the login: the URL is always printed.
// On Linux it only tries when a graphical session is visible (DISPLAY / WAYLAND_DISPLAY): over plain SSH, xdg-open would
// otherwise start a text-mode browser inside this very terminal. A seam for tests.
var openBrowser = func(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux", "freebsd", "openbsd", "netbsd":
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return errors.New("no graphical session")
		}
		cmd = exec.Command("xdg-open", url)
	default:
		return errors.New("unsupported platform")
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// defaultDeviceName is what the approval page shows when --device-name is not given: the hostname.
func defaultDeviceName() string {
	if h, err := os.Hostname(); err == nil && strings.TrimSpace(h) != "" {
		return strings.TrimSpace(h)
	}
	return "unknown-device"
}

// postDeviceJSON POSTs body to an UNAUTHENTICATED device-flow route (no Authorization header) and returns status + body.
func postDeviceJSON(ctx context.Context, hc *http.Client, url string, body interface{}) (int, []byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent())
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, data, err
}

// maxConsecutivePollFailures bounds how long a flaky network can stall a poll loop before giving up.
const maxConsecutivePollFailures = 5

// runDeviceFlow starts a device authorization, shows the code and URL, tries the browser, and polls until the key
// arrives. Returns the token and ExitOK, or nil and the exit code to use (the message is already printed).
func runDeviceFlow(ctx context.Context, baseURL, deviceName, profile string, noBrowser bool, io_ *loginIO) (*deviceToken, int) {
	hc := &http.Client{Timeout: 30 * time.Second}

	status, body, err := postDeviceJSON(ctx, hc, baseURL+"/api/cli/device", map[string]string{
		"client":      "sentinel-cli",
		"version":     buildVersion(),
		"device_name": deviceName,
		"profile":     profile,
	})
	if err != nil {
		fmt.Fprintln(io_.stderr, "error: starting login:", err)
		return nil, ExitNetwork
	}
	if status != 200 {
		var de deviceError
		_ = json.Unmarshal(body, &de)
		fmt.Fprintf(io_.stderr, "error: server returned %d starting login: %s\n", status, firstNonEmpty(de.Description, de.Error, strings.TrimSpace(string(body))))
		if status == 404 {
			fmt.Fprintln(io_.stderr, "this server does not support browser login; ask an admin for an agent key and use `sentinel login --key-stdin`")
		}
		return nil, exitCodeForStatus(status)
	}
	var start deviceStart
	if err := json.Unmarshal(body, &start); err != nil || start.DeviceCode == "" || start.UserCode == "" || start.VerificationURI == "" {
		fmt.Fprintln(io_.stderr, "error: unexpected response from /api/cli/device (is this a Sentinel URL?)")
		return nil, ExitNetwork
	}

	openURL := firstNonEmpty(start.VerificationURIComplete, start.VerificationURI)
	fmt.Fprintf(io_.stderr, "\nTo log in, open this URL in a browser where you are signed in to Sentinel:\n\n    %s\n\nand check that it shows this code:\n\n    %s\n\n", openURL, start.UserCode)
	fmt.Fprintln(io_.stderr, "Only approve it if you started this login yourself, just now.")
	switch {
	case !sameOriginHTTP(openURL, baseURL):
		// The server chose this URL. Only ever hand the OS opener an http(s) URL on the server we were told to log in
		// to; anything else (another host, another scheme) is shown, never opened.
		fmt.Fprintf(io_.stderr, "WARNING: that approval URL is not on %s, so the CLI is not opening it. Check it carefully before visiting it yourself.\n", baseURL)
	case !noBrowser:
		if err := openBrowser(openURL); err != nil {
			fmt.Fprintln(io_.stderr, "(could not open a browser here; open the URL on any device)")
		}
	}
	expiresIn := start.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 600
	}
	fmt.Fprintf(io_.stderr, "Waiting for approval (the code expires in %d minutes; Ctrl-C to cancel)...\n", (expiresIn+59)/60)

	interval := start.Interval
	if interval < 1 {
		interval = 5
	}
	deadline := time.Now().Add(time.Duration(expiresIn) * time.Second)
	failures := 0
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(io_.stderr, "\nlogin cancelled; nothing was saved")
			return nil, ExitUsage
		case <-tick(interval):
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(io_.stderr, "error: the code expired before it was approved; run `sentinel login` again")
			return nil, ExitAuth
		}

		status, body, err := postDeviceJSON(ctx, hc, baseURL+"/api/cli/device/token", map[string]string{"device_code": start.DeviceCode})
		if err != nil || status >= 500 {
			if ctx.Err() != nil {
				fmt.Fprintln(io_.stderr, "\nlogin cancelled; nothing was saved")
				return nil, ExitUsage
			}
			if failures++; failures >= maxConsecutivePollFailures {
				fmt.Fprintf(io_.stderr, "error: polling for approval kept failing (%s)\n", firstNonEmpty(errString(err), fmt.Sprintf("HTTP %d", status)))
				return nil, ExitNetwork
			}
			continue
		}
		failures = 0

		if status == 200 {
			var tok deviceToken
			if err := json.Unmarshal(body, &tok); err != nil || tok.Key == "" {
				fmt.Fprintln(io_.stderr, "error: unexpected response from /api/cli/device/token")
				return nil, ExitNetwork
			}
			return &tok, ExitOK
		}

		var de deviceError
		_ = json.Unmarshal(body, &de)
		switch de.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			// RFC 8628 §3.5: add 5 s; the server may tell us the exact interval (or a Retry-After on 429).
			switch {
			case de.Interval > interval:
				interval = de.Interval
			case de.RetryAfter > 0:
				interval = de.RetryAfter
			default:
				interval += 5
			}
			continue
		case "access_denied":
			fmt.Fprintln(io_.stderr, "error: the login was denied in the browser; nothing was saved")
			return nil, ExitAuth
		case "expired_token":
			fmt.Fprintln(io_.stderr, "error: the code expired before it was approved; run `sentinel login` again")
			return nil, ExitAuth
		default:
			fmt.Fprintf(io_.stderr, "error: login failed (%d %s): %s\n", status, de.Error, firstNonEmpty(de.Description, strings.TrimSpace(string(body))))
			return nil, exitCodeForStatus(status)
		}
	}
}

// sameOriginHTTP reports whether raw is an http(s) URL with exactly base's scheme and host (host includes the port).
func sameOriginHTTP(raw, base string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	b, err := url.Parse(base)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	return (scheme == "http" || scheme == "https") &&
		scheme == strings.ToLower(b.Scheme) &&
		u.Host != "" && strings.EqualFold(u.Host, b.Host) && u.User == nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
