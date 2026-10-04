#!/usr/bin/env python3
"""kubehz-api-stub.py: an https stub of the kubehz api for hack/parity-kubehz.sh.

It serves the routes of `lo kubehz space ...` and `lo kubehz cluster ...`
plus the agent-key token endpoint, with fixed answers in the api's shapes
(the {ok, data, traceId} envelope, the {statusCode, data: {code, message,
help}} refusal). Every request is appended to a log as one JSON line:
method, path, whether the bearer was the minted token, and the body (parsed
JSON with sorted keys, so a different key order is not a difference). The
harness diffs that log between the two implementations.

Usage: kubehz-api-stub.py <cert.pem> <key.pem> <port-file> <request-log>
It binds 127.0.0.1 on a free port and writes the port to <port-file>.
"""

import http.server
import json
import os
import ssl
import sys

CERT, KEY, PORT_FILE, LOG = sys.argv[1:5]
TOKEN = "parity-jwt"

SPACE = {
    "id": "sp-1a2b3c4d", "tenantId": "t-1", "shardId": "sh-1", "name": "Acme Prod", "slug": "acme",
    "status": "Active", "maxNodes": 2, "maxNamespaces": 1, "maxObjectKiB": 256,
    "limitsCeiling": {"nodes": 5}, "leaseExpiresAt": "2026-10-04T14:00:00.000Z",
    "createdAt": "2026-10-04T12:00:00.000Z", "updatedAt": None, "namespaces": ["acme"], "nodeCount": 1,
}
# Hostile strings: an ANSI escape, a backslash, a line separator, a word YAML
# reads as a bool, a number-like slug, no lease, a missing field.
ODD_SPACE = {
    "id": "sp-9z9z9z9z", "name": "Ev\u001b[2Jil \\ na\u2028me \u00fc\u2713", "slug": "yes",
    "status": "Pending", "maxNodes": 1, "maxNamespaces": 3, "maxObjectKiB": None,
    "leaseExpiresAt": None, "createdAt": "2026-10-04T12:30:00.000Z", "namespaces": ["yes", 7], "nodeCount": 0,
}
SPACE_DETAIL = {
    **{k: v for k, v in SPACE.items() if k not in ("namespaces", "nodeCount")},
    "namespaces": [{"name": "acme", "createdAt": "2026-10-04T12:00:00.000Z"}],
    "nodes": [{"name": "worker-1", "lane": "metal", "status": "Ready", "joinedAt": None, "lastSeenAt": None,
               "kubeletVersion": "v1.34.1"}],
    "usage": {"nodes": 1, "nodesReady": 1, "maxNodes": 2, "namespaces": 1, "maxNamespaces": 1},
    "endpoint": "https://acme.k8s.kubehz.example", "ownCRDs": False, "controlPlane": None,
}
CLUSTER = {
    "id": "cl-1a2b3c4d", "tenantId": "t-1", "domain": "agent.example.org", "kind": "KubeOne", "hosting": "hosted",
    "access": "registered", "status": "Running", "provider": "hetzner", "region": "fsn1",
    "kubernetesVersion": "v1.34.1", "controlPlaneReplicas": 1, "apiEndpoint": "https://203.0.113.7:6443",
    "health": "healthy", "leaseExpiresAt": "2026-10-04T14:00:00.000Z", "createdAt": "2026-10-04T12:00:00.000Z",
    "workers": [],
}
SELF_CLUSTER = {
    "id": "cl-self0001", "domain": "self.example.org", "hosting": "self", "status": "active", "region": "nbg1",
    "kubernetesVersion": None, "controlPlaneReplicas": 3, "apiEndpoint": None, "health": "unknown",
    "leaseExpiresAt": None, "createdAt": "2026-10-03T08:00:00.000Z",
}
KUBECONFIG = """apiVersion: v1
kind: Config
clusters:
  - name: kubehz-acme
    cluster:
      server: https://acme.k8s.kubehz.example
users:
  - name: kubehz-acme-agent
    user:
      exec:
        apiVersion: client.authentication.k8s.io/v1
        command: lo
        args: [kubehz, token, --token-url, "https://id.kubehz.example/oauth/v2/token", --scope, "s"]
        interactiveMode: Never
contexts:
  - name: kubehz-acme
    context: {cluster: kubehz-acme, user: kubehz-acme-agent, namespace: acme}
current-context: kubehz-acme
"""


def ok(data, meta=None):
    body = {"ok": True, "data": data, "traceId": "tr-1"}
    if meta is not None:
        body["meta"] = meta
    return body


def refusal(status, code, message, help_text=None):
    data = {"code": code, "message": message, "traceId": "tr-1"}
    if help_text is not None:
        data["help"] = help_text
    return status, {"statusCode": status, "statusMessage": "", "message": message, "data": data}


NOT_FOUND = refusal(404, "NOT_FOUND", "Space not found", "Check the space id, or list your spaces with GET /api/spaces.")

ROUTES = {
    ("GET", "/api/spaces"): (200, ok([SPACE, ODD_SPACE], {"pagination": {"page": 1, "perPage": 500, "total": 2, "pages": 1}})),
    ("GET", "/api/spaces/sp-1a2b3c4d"): (200, ok(SPACE_DETAIL)),
    ("GET", "/api/spaces/sp-gone0001"): NOT_FOUND,
    ("GET", "/api/spaces/sp-scoped01"): refusal(403, "AGENT_KEY_OUT_OF_SCOPE", "A resource-scoped agent key cannot read tenant-wide data",
                                                "Use an agent key with the scope \"tenant\" for this endpoint."),
    ("GET", "/api/spaces/sp-broken01"): (200, "<html>gateway</html>"),
    ("GET", "/api/spaces/sp-boom0001"): (502, ""),
    ("GET", "/api/spaces/sp-hostile1"): refusal(418, "not a code", "Te\u001b]0;pwned\u0007a \\e[31m time", "\u001b[2JHelp \\033[0m"),
    ("DELETE", "/api/spaces/sp-1a2b3c4d"): (200, ok({"id": "sp-1a2b3c4d", "status": "Deleting"})),
    ("DELETE", "/api/spaces/sp-gone0001"): NOT_FOUND,
    ("PATCH", "/api/spaces/sp-1a2b3c4d/lease"): (200, ok({"id": "sp-1a2b3c4d", "leaseExpiresAt": "2026-10-05T12:00:00.000Z"})),
    ("PATCH", "/api/spaces/sp-capped01/lease"): refusal(409, "AGENT_KEY_SPEND_CAP", "This agent key spent 1200 of its 1000 cents this month",
                                                         "Delete what the key created, or ask an owner for a key with a higher cap."),
    ("GET", "/api/spaces/sp-1a2b3c4d/kubeconfig/agent"): (200, KUBECONFIG),
    ("GET", "/api/spaces/sp-dedicat1/kubeconfig/agent"): refusal(
        409, "SPACE_HAS_DEDICATED_CONTROL_PLANE",
        "This space runs a dedicated control plane: download its kubeconfig from the control-plane route",
        "GET /api/spaces/sp-dedicat1/control-plane/kubeconfig (admin) or ?variant=sso (kubelogin)."),
    ("GET", "/api/clusters"): (200, ok([CLUSTER, SELF_CLUSTER], {"pagination": {"page": 1, "perPage": 500, "total": 612, "pages": 2}})),
    ("GET", "/api/clusters/cl-1a2b3c4d"): (200, ok({**CLUSTER, "heartbeat": {}, "execution": {}})),
    ("PATCH", "/api/clusters/cl-1a2b3c4d/lease"): (200, ok({"id": "cl-1a2b3c4d", "leaseExpiresAt": "2026-10-05T12:00:00.000Z"})),
    ("PATCH", "/api/clusters/cl-self0001/lease"): refusal(400, "LEASE_HOSTED_ONLY", "A lease applies to hosted clusters only",
                                                          "A self-hosted cluster runs on your own account: kubehz never deletes it on a timer."),
    ("PATCH", "/api/clusters/cl-viewer01/lease"): refusal(403, "TOKEN_SCOPE_MISSING", "This endpoint requires the 'clusters:write' scope",
                                                          "An agent key with the role 'viewer' has no 'clusters:write' scope."),
    ("GET", "/api/clusters/cl-1a2b3c4d/kubeconfig/agent"): (200, KUBECONFIG),
    ("GET", "/api/clusters/cl-pending1/kubeconfig/agent"): refusal(409, "KUBECONFIG_NOT_READY", "Kubeconfig is not ready yet — the control plane is still provisioning",
                                                                   "Poll GET /clusters/cl-pending1 until phase is \"Running\", then retry."),
}

# POST /api/spaces answers by the slug it was sent.
CREATES = {
    "acme": (201, ok({**SPACE, "status": "Pending", "nodeCount": 0, "leaseExpiresAt": "2026-10-04T14:00:00.000Z"})),
    "taken": refusal(409, "SPACE_EXISTS", "A space with that name or URL slug already exists",
                     "Pick a different slug — it must be unique across the platform because it is a DNS name."),
    "big": refusal(403, "SPACE_LIMITS_ABOVE_FREE", "The requested limits are above this account's ceiling (nodes 1)",
                   "Add a payment method, or lower the values."),
    "full": refusal(409, "NO_SHARD_AVAILABLE", "No shared control plane has room right now",
                    "This is a platform capacity limit, not an account limit."),
    "capped": refusal(409, "AGENT_KEY_SPEND_CAP", "This agent key spent 1200 of its 1000 cents this month",
                      "Delete what the key created, or ask an owner for a key with a higher cap."),
}


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def answer(self, status, body):
        if isinstance(body, (dict, list)):
            raw, ctype = json.dumps(body).encode(), "application/json"
        else:
            raw = body.encode()
            ctype = "application/yaml" if body.startswith("apiVersion") else "text/html"
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def route(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length).decode() if length else ""
        try:
            body = json.loads(raw) if raw and self.path != "/oauth/v2/token" else raw
        except ValueError:
            body = raw
        auth = self.headers.get("Authorization", "")
        with open(LOG, "a", encoding="utf-8") as log:
            log.write(json.dumps({"method": self.command, "path": self.path,
                                  "bearer": auth == "Bearer " + TOKEN if self.path != "/oauth/v2/token" else None,
                                  "accept": self.headers.get("Accept"), "body": body}, sort_keys=True) + "\n")
        if self.path == "/oauth/v2/token":
            return self.answer(200, {"access_token": TOKEN, "token_type": "Bearer", "expires_in": 3600})
        if auth != "Bearer " + TOKEN:
            return self.answer(*refusal(401, "UNAUTHORIZED", "Invalid or expired agent key",
                                        "The agent key is revoked or expired. Create a new one under Settings -> Agent keys."))
        path = self.path.split("?", 1)[0]
        if self.command == "POST" and path == "/api/spaces" and isinstance(body, dict):
            return self.answer(*CREATES.get(body.get("slug"), refusal(400, "BAD_REQUEST", "slug: invalid")))
        return self.answer(*ROUTES.get((self.command, path), refusal(404, "NOT_FOUND", "Not found")))

    do_GET = do_POST = do_PATCH = do_DELETE = route


def main():
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain(CERT, KEY)
    server.socket = ctx.wrap_socket(server.socket, server_side=True)
    with open(PORT_FILE + ".tmp", "w", encoding="utf-8") as f:
        f.write(str(server.server_address[1]))
    os.replace(PORT_FILE + ".tmp", PORT_FILE)
    server.serve_forever()


if __name__ == "__main__":
    main()
