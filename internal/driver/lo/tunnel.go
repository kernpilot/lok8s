package lo

// tunnel.go — SSH tunnel + kubeconfig rewrite for remote API access
// (.lok8s/drivers/lo/utils/tunnel.sh).

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/ui"
)

// kubeconfigTunnel opens an SSH tunnel for the k8s API and rewrites the
// kubeconfig to localhost (bash: lo::kubeconfig_tunnel). The server rewrite
// happens REGARDLESS of tunnel success — the bash behavior: a failed
// port-forward warns, and the kubeconfig still points at 127.0.0.1 (the
// operator re-runs the tunnel; a remote-IP server would never work from the
// workstation anyway).
func (d *Driver) kubeconfigTunnel(ctx context.Context, kubeconfigPath, remoteUser, remoteIP string, errOut io.Writer) error {
	raw, err := os.ReadFile(kubeconfigPath)
	if err != nil {
		return err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return err
	}
	serverNode := kubeconfigServerNode(&root)
	currentServer := ""
	if serverNode != nil {
		currentServer = serverNode.Value
	}

	port := currentServer[strings.LastIndex(currentServer, ":")+1:]
	remoteHost := strings.TrimPrefix(currentServer, "https://")
	if i := strings.LastIndex(remoteHost, ":"); i >= 0 {
		remoteHost = remoteHost[:i]
	}

	if err := d.runQuiet(ctx, "ssh", "-fN",
		"-o", "ServerAliveInterval=15",
		"-L", fmt.Sprintf("%s:%s:%s", port, remoteHost, port),
		remoteUser+"@"+remoteIP); err != nil {
		ui.Warnf(errOut, "SSH port-forward for API failed — kubeconfig may not be reachable locally")
	}

	if serverNode != nil {
		serverNode.Value = "https://127.0.0.1:" + port
		serverNode.Style = 0
		out, err := yaml.Marshal(&root)
		if err != nil {
			return err
		}
		if err := os.WriteFile(kubeconfigPath, out, 0o600); err != nil {
			return err
		}
	}
	ui.Debugf(errOut, "API tunnel: localhost:%s → %s:%s", port, remoteHost, port)
	return nil
}

// kubeconfigServerNode finds .clusters[0].cluster.server in a kubeconfig
// document (the same path the bash yq expressions addressed).
func kubeconfigServerNode(root *yaml.Node) *yaml.Node {
	clusters := ylookup(root, "clusters")
	if clusters == nil || clusters.Kind != yaml.SequenceNode || len(clusters.Content) == 0 {
		return nil
	}
	return ylookup(clusters.Content[0], "cluster", "server")
}
