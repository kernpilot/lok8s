package kubehz

// fingerprint.go — kubehz::get_ssh_fingerprint: the identifier a cluster
// announces itself with. kubehz-core matches it against the SSH keys in the
// user's Hetzner Cloud account, and Hetzner exposes MD5 fingerprints — so
// the output is the MD5 form (`ssh-keygen -E md5`), never SHA256.

import (
	"context"
	"os"
	"strings"

	"github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/execx"
)

// SSHFingerprint ports kubehz::get_ssh_fingerprint. The method varies by
// cluster kind: kubeone hashes the provider's/spec's public key file, capi
// asks hcloud for the named key, lo has no SSH key and uses "lo:<domain>".
func (c *Context) SSHFingerprint(ctx context.Context, clusterYAML string) (string, error) {
	kind, err := domain.SpecDriver(clusterYAML, "")
	if err != nil {
		kind = ""
	}
	doc := loadSpec(clusterYAML)

	switch kind {
	case "kubeone":
		// Read the SSH public key from the provider output access[], fall
		// back to the spec.
		keyFile := ""
		if c.ProviderOutput != nil {
			if raw, err := c.ProviderOutput(ctx); err == nil {
				if v, ok := parseJSON(raw); ok {
					if acc, ok := jget(v, "access").([]any); ok && len(acc) > 0 {
						keyFile = jstrOr(acc[0], "", "publicKey")
					}
				}
			}
		}
		if keyFile == "" {
			keyFile = doc.or("~/.ssh/id_ed25519.pub", "spec", "hcloud", "sshPublicKeyFile")
		}
		if strings.HasPrefix(keyFile, "~") {
			keyFile = os.Getenv("HOME") + keyFile[1:]
		}
		out, err := c.capture(ctx, false, "ssh-keygen", "-lf", keyFile, "-E", "md5")
		if err != nil {
			return "", err
		}
		return awkField(out, 2), nil
	case "capi":
		keyName := doc.or("", "spec", "hcloud", "sshKeyName")
		if keyName == "" {
			// bash: the case arm runs nothing and returns 0 — an empty
			// fingerprint.
			return "", nil
		}
		out, err := c.capture(ctx, false, "hcloud", "ssh-key", "describe", keyName, "-o", "json")
		if err != nil {
			return "", err
		}
		v, _ := parseJSON([]byte(out))
		pubKey := jstr(jget(v, "public_key"))
		fp, err := c.sshKeygenStdin(ctx, pubKey+"\n")
		if err != nil {
			return "", err
		}
		return fp, nil
	case "lo":
		// Lo clusters don't have SSH keys — use the cluster domain as
		// identifier.
		return "lo:" + doc.or("", "spec", "cluster", "domain"), nil
	default:
		c.warnf("Cannot extract SSH fingerprint for kind=%s", kind)
		return "", ErrHandled
	}
}

// sshKeygenStdin is `echo "$pub" | ssh-keygen -lf /dev/stdin -E md5
// 2>/dev/null | awk '{print $2}'`.
func (c *Context) sshKeygenStdin(ctx context.Context, pubKey string) (string, error) {
	var stdout strings.Builder
	err := c.Runner.Run(ctx, execx.Cmd{
		Name:   "ssh-keygen",
		Args:   []string{"-lf", "/dev/stdin", "-E", "md5"},
		Stdin:  strings.NewReader(pubKey),
		Stdout: &stdout,
		Stderr: discardWriter{},
	})
	if err != nil {
		return "", err
	}
	return awkField(stdout.String(), 2), nil
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// awkField is `awk '{print $N}'` over the first line: whitespace-split,
// 1-based, "" when absent.
func awkField(out string, n int) string {
	line := out
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	fields := strings.Fields(line)
	if n-1 < len(fields) {
		return fields[n-1]
	}
	return ""
}
