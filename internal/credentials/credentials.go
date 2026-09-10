// Package credentials holds the two guards every driver runs before it
// talks to a cloud API: the provider credential presence check
// (bash: utils/credentials.sh, credentials::require) and the HTTPS-only
// URL check (bash: utils/http.sh, http::require_https). Bearer tokens
// travel on these calls, never over plain HTTP.
package credentials

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kernpilot/lok8s/internal/ui"
)

// Require checks the environment variables a provider needs. Every missing
// variable is reported before the check fails; an unknown provider fails
// with its own line. The returned error is already printed (ui.Handled).
func Require(provider string, stderr io.Writer) error {
	var missing []string
	switch provider {
	case "hetzner":
		if os.Getenv("HCLOUD_TOKEN") == "" {
			missing = append(missing, "HCLOUD_TOKEN")
		}
	case "aws":
		if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
			missing = append(missing, "AWS_ACCESS_KEY_ID")
		}
		if os.Getenv("AWS_SECRET_ACCESS_KEY") == "" {
			missing = append(missing, "AWS_SECRET_ACCESS_KEY")
		}
	default:
		ui.ErrorTo(stderr, "unknown provider '%s' for credential check", provider)
		return ui.Handled(fmt.Errorf("credentials: unknown provider %q for credential check", provider))
	}
	if len(missing) > 0 {
		for _, v := range missing {
			ui.ErrorTo(stderr, "required environment variable %s is not set", v)
		}
		return ui.Handled(fmt.Errorf("credentials: missing credentials: %s", strings.Join(missing, ", ")))
	}
	return nil
}

// RequireHTTPS fails when url does not start with https://. label names
// the URL in the error lines. The returned error is already printed.
func RequireHTTPS(url, label string, stderr io.Writer) error {
	if !strings.HasPrefix(url, "https://") {
		ui.ErrorTo(stderr, "%s must use HTTPS: %s", label, url)
		ui.ErrorTo(stderr, "Plain HTTP is not allowed for security reasons")
		return ui.Handled(fmt.Errorf("credentials: %s must use HTTPS: %s", label, url))
	}
	return nil
}
