#!/usr/bin/env python3
"""kubehz-api-stub.py: an https stub of the kubehz api for hack/parity-kubehz.sh.

One server for the two sections of the harness that need an api:

- The agent tools: the routes of `lo kubehz space ...` and `lo kubehz
  cluster ...` and the agent-key token endpoint, with fixed answers in the
  api's shapes (the {ok, data, traceId} envelope, the {statusCode, data:
  {code, message, help}} refusal). These routes need the minted bearer.
- The node maintenance cases: `lo kubehz node cordon|uncordon|drain` on
  the nodes of SPACE (POST /api/spaces/sp-1a2b3c4d/nodes/<name>/<action>,
  answered by the node name: an accepted request, the 202 that names a
  replaced request, each refusal code of the three routes, the answers of a
  gateway, a 2xx that is not the answer of the route, and a connection that
  ends with no answer), and the node list that `lo kubehz status` reads,
  with the cordon and maintenance fields. These routes need the minted
  bearer too (KUBEHZ_TOKEN=parity-jwt).
- The register cases: POST /api/clusters/register answers one response that
  fits every register mode and hands out a new bind secret; its claim key
  name is kubehz-claim-<domain of the body>. The Hetzner calls of the
  claim-key mode and of deregister (/v1/ssh_keys) get the key that SSH_KEYS
  holds for the name (none for any other name), a created key, and a
  deleted key. These routes take any credential. A register body for a
  domain in UNAVAILABLE gets that refusal instead, every time (#312): 503 or
  429 with a Retry-After of zero seconds (so the retries do not wait; "00"
  checks that a leading zero reads as decimal), a 503 without Retry-After,
  one with the date form, and one whose code is an escape sequence.

Every request is appended to a log as one JSON line: the method, the path,
the credential (none, the minted bearer, another bearer, or another scheme;
never its value), the Accept header ("*/*" logs as no header) and the
Content-Type header, and the body (parsed
JSON with sorted keys, so a different key order is not a difference). The
harness diffs that log between the two implementations.

POST /oauth/v2/redirect answers 307 to the token endpoint: a grant that
followed it would hand the client secret to another URL. GET
/api/spaces/sp-race0001/kubeconfig/agent writes "planted" to the file that
STUB_RACE_FILE names before it answers: a file that appears during the
download.

Usage: kubehz-api-stub.py <cert.pem> <key.pem> <port-file> <request-log>
It binds 127.0.0.1 on a free port and writes the port to <port-file>.
"""

import http.server
import json
import os
import ssl
import sys
import urllib.parse

CERT, KEY, PORT_FILE, LOG = sys.argv[1:5]
TOKEN = "parity-jwt"
# The bind secret that a register hands out; lo sends it on the next register.
NEXT_BIND = "009c2b3a4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70"
REGISTERED = {"id": "cl-parity", "registered": True, "claimed": True, "bindSecret": NEXT_BIND,
              "claimKey": {"publicKey": "ssh-ed25519 AAAA k", "fingerprint": "aa:bb",
                           "name": "kubehz-claim-bind-reg.dev"}}
# Register answers whose bindSecret is no bind secret, by the domain of the
# body: a final LF, a NUL inside, upper case, and a number of 64 digits. lo
# keeps the stored .kubehz-bind as it is and warns.
BAD_BIND = {
    "bad-lf.dev": NEXT_BIND + "\n",
    "bad-nul.dev": NEXT_BIND[:32] + "\x00" + NEXT_BIND[32:],
    "bad-upper.dev": NEXT_BIND.upper(),
    "bad-number.dev": int("1" * 64),
}


# The value of the label lok8s.dev/kubehz-claim on the claim key that lo
# made for own-key.dev: the first 32 hex characters of the SHA-256 of the
# domain, `printf %s own-key.dev | sha256sum | cut -c1-32`. A literal, so a
# reader can check the value. The literal cases of the bats and Go unit
# tests pin the mark itself. This harness compares the bash implementation
# with the Go implementation only: when both calculate the mark another way,
# own-key.dev is a foreign key for both, and the harness does not see it.
OWN_MARK = "303972d926cdaf0cc2869d244c134014"


# GET /v1/ssh_keys?name= answers by the key name: the key that lo made for
# own-key.dev, a key without labels (a person's, or one of an older lo),
# and a key with the label of another cluster.
SSH_KEYS = {
    "kubehz-claim-own-key.dev": {"id": 42, "name": "kubehz-claim-own-key.dev",
                                 "labels": {"lok8s.dev/kubehz-claim": OWN_MARK}},
    "kubehz-claim-legacy-key.dev": {"id": 42, "name": "kubehz-claim-legacy-key.dev"},
    "kubehz-claim-other-key.dev": {"id": 42, "name": "kubehz-claim-other-key.dev",
                                   "labels": {"lok8s.dev/kubehz-claim": OWN_MARK}},
}

# POST /api/clusters/register refusals by the domain of the body (#312):
# (status, Retry-After or None, code).
UNAVAILABLE = {
    "busy-reg.dev": (503, "0", "REGISTRATION_UNAVAILABLE"),
    "limit-reg.dev": (429, "00", "RATE_LIMITED"),
    "bare-reg.dev": (503, None, "REGISTRATION_UNAVAILABLE"),
    "date-reg.dev": (503, "Wed, 21 Oct 2026 07:28:00 GMT", "REGISTRATION_UNAVAILABLE"),
    "odd-reg.dev": (503, "0", "\u001b[2JPWNED"),
}

SPACE = {
    "id": "sp-1a2b3c4d", "tenantId": "t-1", "shardId": "sh-1", "name": "Acme Prod", "slug": "acme",
    "status": "Active", "maxNodes": 2, "maxNamespaces": 1, "maxObjectKiB": 256,
    "limitsCeiling": {"nodes": 5}, "leaseExpiresAt": "2026-10-04T14:00:00.000Z",
    "createdAt": "2026-10-04T12:00:00.000Z", "updatedAt": None, "namespaces": ["acme"], "nodeCount": 1,
}
# Hostile strings: an ANSI escape, a backslash, a line separator, C1 and bidi
# controls, a word YAML reads as a bool, no lease, a missing field.
ODD_SPACE = {
    "id": "sp-9z9z9z9z", "name": "Ev\u001b[2Jil \\ na\u2028me\u0085\u009b\u202e\u2066 \u00fc\u2713", "slug": "yes",
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

# GET /api/spaces/sp-1a2b3c4d/nodes: the node list of SPACE with the fields
# `lo kubehz status` shows after the lane: a cordon, the last maintenance
# request and its phase, a reason with an escape sequence, a reason longer
# than 256 code points (in two-byte characters), and a request whose action
# and phase are not strings.
SPACE_NODES = {
    "nodes": [
        {"name": "worker-1", "lane": "metal", "status": "Ready", "joinedAt": None, "lastSeenAt": None,
         "kubeletVersion": "v1.34.1", "unschedulable": True,
         "maintenance": {"action": "drain", "phase": "Blocked", "since": "2026-10-04T12:10:00.000Z",
                         "reason": "2 pods have no controller \u001b[31mand the drain has no force"}},
        {"name": "worker-2", "lane": "hcloud", "status": "Ready", "joinedAt": None, "lastSeenAt": None,
         "kubeletVersion": "v1.34.1", "unschedulable": False,
         "maintenance": {"action": "uncordon", "phase": "Done", "since": "2026-10-04T12:11:00.000Z"}},
        {"name": "worker-3", "lane": "metal", "status": "NotReady", "joinedAt": None, "lastSeenAt": None,
         "kubeletVersion": None, "unschedulable": True, "maintenance": None},
        {"name": "worker-4", "lane": "metal", "status": "Ready", "joinedAt": None, "lastSeenAt": None,
         "kubeletVersion": "v1.34.1", "unschedulable": False,
         "maintenance": {"phase": 7, "since": "2026-10-04T12:12:00.000Z", "reason": "\u00fc" * 300}},
        {"name": "joining-1", "lane": "unknown", "status": "Joining", "joinedAt": None, "lastSeenAt": None,
         "kubeletVersion": None, "unschedulable": False, "maintenance": None},
        # A newline and a tab in an action and a reason: both become a
        # space, so the reason cannot print a node line of its own.
        {"name": "worker-6", "lane": "metal", "status": "Ready", "joinedAt": None, "lastSeenAt": None,
         "kubeletVersion": "v1.34.1", "unschedulable": False,
         "maintenance": {"action": "drain\tx", "phase": "Failed", "since": "2026-10-04T12:13:00.000Z",
                         "reason": "no\n  worker-9  Ready  metal\tforged"}},
    ],
    "usage": {"nodes": 6, "nodesReady": 4, "maxNodes": 6},
    "kubelet": {"oldest": "v1.34.1", "floor": "v1.32", "belowFloor": False},
}


def accepted(name, action, replaced=None):
    return 202, ok({"name": name, "action": action, "status": "requested", "replaced": replaced})


# DROP: read the request, then close the connection with no answer (a
# transport error after the send).
DROP = object()

# POST /api/spaces/sp-1a2b3c4d/nodes/<name>/<action>, by (name, action). A
# pair that is not here answers 404 NOT_FOUND (no such node).
MAINTENANCE = {
    ("worker-1", "cordon"): accepted("worker-1", "cordon"),
    ("worker-1", "uncordon"): accepted("worker-1", "uncordon", "drain"),
    ("worker-1", "drain"): accepted("worker-1", "drain"),
    ("worker-2", "drain"): accepted("worker-2", "drain", "drain"),
    ("worker-2", "cordon"): accepted("worker-2", "cordon", "drain"),
    ("worker-2", "uncordon"): accepted("worker-2", "uncordon", "cordon"),
    # A 2xx that is not the answer of the route (not JSON, or no status
    # "requested"): the result is open.
    ("html-1", "cordon"): (202, "<html>accepted</html>"),
    ("odd-1", "cordon"): (202, ok({"name": "odd-1", "action": "cordon", "status": "queued", "replaced": None})),
    # A gateway in front of the api: a timeout, a bad gateway, and a 503
    # with no code. The result is open.
    ("worker-10", "drain"): (504, "upstream request timeout"),
    ("worker-11", "cordon"): (502, ""),
    ("worker-12", "uncordon"): (503, "no healthy upstream"),
    ("drop-1", "cordon"): DROP,
    ("joining-1", "cordon"): refusal(409, "NODE_NOT_JOINED", "The node has not joined the space yet",
                                     "Nothing changed. Wait until GET /api/spaces/{id}/nodes shows the node Ready, then retry."),
    ("worker-3", "drain"): refusal(503, "NODE_MAINTENANCE_OUTCOME_UNKNOWN", "The outcome of the node maintenance request is unknown",
                                   "Read the node's `maintenance` status before you retry."),
    ("worker-4", "drain"): refusal(403, "STEP_UP_REQUIRED", "A fresh sign-in is required"),
    ("worker-5", "cordon"): refusal(503, "NODE_MAINTENANCE_UNAVAILABLE", "The node maintenance request did not reach the control plane",
                                    "Nothing changed on the node. Retry shortly."),
    ("worker-6", "cordon"): refusal(409, "NODE_MAINTENANCE_CONFLICT", "A maintenance request of another space holds this request name",
                                    "Nothing changed. Contact support."),
    ("worker-7", "cordon"): refusal(503, "NODE_MAINTENANCE_AUDIT_UNAVAILABLE", "The node maintenance request cannot be recorded",
                                    "Nothing changed on the node: the request is not sent without its audit record. Retry shortly."),
    ("worker-8", "uncordon"): refusal(409, "LEASE_ENDED", "The lease of this space ended",
                                      "After the end, the api refuses every change except the delete."),
    ("worker-9", "cordon"): refusal(403, "TOKEN_SCOPE_MISSING", "This endpoint requires the 'clusters:write' scope"),
    ("hostile-1", "cordon"): refusal(400, "BAD_REQUEST", "Refused \u001b[2J by \\e[31m policy", "\u001b[HAsk \\033[0m support"),
}
MAINTENANCE_PREFIX = "/api/spaces/sp-1a2b3c4d/nodes/"

# POST /api/spaces/sp-1a2b3c4d/join-token, by the node name in the body,
# with no join script: a ticket in the bootstrap-token shape, and three
# that both implementations refuse (a trailing newline, a NUL, a CR). The
# ticket is built here from two parts.
JOIN_PATH = "/api/spaces/sp-1a2b3c4d/join-token"
TICKET = ".".join(("a1b2c3", "d4e5f6g7h8i9j0k1"))
TICKETS = {"worker-1": TICKET, "worker-nl": TICKET + "\n", "worker-nul": TICKET + "\x00",
           "worker-cr": TICKET + "\r"}


def retry_later(code, message, help_text, seconds):
    """A 503 refusal with retryAfterSeconds in its data and a Retry-After
    header, or neither when seconds is None."""
    status, body = refusal(503, code, message, help_text)
    if seconds is None:
        return status, body
    body["data"]["retryAfterSeconds"] = seconds
    return status, body, {"Retry-After": str(seconds)}


class RawJSON(str):
    """A JSON body that the stub sends as written: json.dumps never writes
    a number as 5e0 or 1E3."""


def retry_later_raw(code, message, help_text, number):
    """A 503 refusal whose retryAfterSeconds is the JSON number text as
    given (5e0, 1E3, 36.01e2, 1e400), with no Retry-After header."""
    status, body = refusal(503, code, message, help_text)
    body["data"]["retryAfterSeconds"] = "@WAIT@"
    return status, RawJSON(json.dumps(body).replace('"@WAIT@"', number))


# DELETE /api/clusters/cl-1a2b3c4d/nodes/<name>: the 503 refusals of a node
# remove on a hosted cluster.
REMOVE_PREFIX = "/api/clusters/cl-1a2b3c4d/nodes/"
REMOVE_HELP = "Nothing was removed. Retry after the Retry-After interval."
REMOVE_OPEN_HELP = "Read the node list of the cluster. If the node is still there, retry the removal after the Retry-After interval."
REMOVES = {
    "remove-1": retry_later("NODE_NOT_REMOVED", "The node was not removed: its join ticket was not revoked", REMOVE_HELP, 5),
    "remove-2": retry_later("NODE_REMOVE_RESULT_UNKNOWN", "The result of this removal is not known", REMOVE_OPEN_HELP, 5),
    "remove-3": retry_later("NODE_NOT_REMOVED", "The node was not removed", REMOVE_HELP, None),
    "remove-4": retry_later("NODE_REMOVE_RESULT_UNKNOWN", "The result of this removal is not known", REMOVE_OPEN_HELP, 1),
    "remove-5": retry_later("NODE_NOT_REMOVED", "The node was not removed", REMOVE_HELP, "5"),
    "remove-6": retry_later("NODE_NOT_REMOVED", "The node was not removed", REMOVE_HELP, 3601),
    "remove-7": retry_later("JOIN_TICKET_BUSY", "Other join tickets of this account are being minted or removed now", REMOVE_HELP, 5),
    # json.dumps writes 1000.0 and 5.5 as they are: a whole value in the
    # form of a fraction, and a fraction.
    "remove-8": retry_later("NODE_NOT_REMOVED", "The node was not removed", REMOVE_HELP, 1000.0),
    "remove-9": retry_later("NODE_NOT_REMOVED", "The node was not removed", REMOVE_HELP, 5.5),
    # Number forms that json.dumps does not write, as the body gives them.
    "remove-10": retry_later_raw("NODE_NOT_REMOVED", "The node was not removed", REMOVE_HELP, "5e0"),
    "remove-11": retry_later_raw("NODE_NOT_REMOVED", "The node was not removed", REMOVE_HELP, "1E3"),
    "remove-12": retry_later_raw("NODE_NOT_REMOVED", "The node was not removed", REMOVE_HELP, "36.01e2"),
    "remove-13": retry_later_raw("NODE_NOT_REMOVED", "The node was not removed", REMOVE_HELP, "1e400"),
}

# POST /api/clusters/cl-1a2b3c4d/nodes/join-token, by the node name in the
# body: the 503 refusals of a join mint on a hosted cluster.
MINT_PATH = "/api/clusters/cl-1a2b3c4d/nodes/join-token"
MINT_HELP = "Mint a new ticket for this node name after the Retry-After interval."
MINTS = {
    "mint-1": retry_later("JOIN_TICKET_NOT_MINTED", "No join ticket was minted: the ticket of this request is not live",
                          MINT_HELP, 5),
    "mint-2": retry_later("JOIN_TICKET_RESULT_UNKNOWN", "The result of this mint is not known",
                          MINT_HELP + " The new mint revokes the ticket of this request if it was kept.", 5),
    "mint-3": retry_later("JOIN_TICKET_BUSY", "Other join tickets of this account are being minted now",
                          "Nothing was minted. Retry after the Retry-After interval.", None),
}

ROUTES = {
    **{("DELETE", REMOVE_PREFIX + name): answer for name, answer in REMOVES.items()},
    ("GET", "/api/spaces"): (200, ok([SPACE, ODD_SPACE], {"pagination": {"page": 1, "perPage": 500, "total": 2, "pages": 1}})),
    ("GET", "/api/spaces/sp-1a2b3c4d"): (200, ok(SPACE_DETAIL)),
    ("GET", "/api/spaces/sp-1a2b3c4d/nodes"): (200, ok(SPACE_NODES)),
    ("GET", "/api/spaces/sp-gone0001"): NOT_FOUND,
    ("GET", "/api/spaces/sp-broken01"): (200, "<html>gateway</html>"),
    ("GET", "/api/spaces/sp-boom0001"): (502, ""),
    ("GET", "/api/spaces/sp-hostile1"): refusal(418, "not a code", "Te\u001b]0;pwned\u0007a \\e[31m\n\ttime", "\u001b[2JHelp\t\\033[0m\n"),
    # A message and a help that are not strings: lo treats both as absent.
    ("GET", "/api/spaces/sp-numeric1"): (400, {"statusCode": 400, "data": {"code": "BAD_REQUEST", "message": 5, "help": {"a": 1}}}),
    # A KUBEHZ_TOKEN with clusters:write but no read (kubehz-api api-tokens.ts).
    ("GET", "/api/spaces/sp-noread01"): refusal(403, "TOKEN_SCOPE_MISSING", "This endpoint requires the 'read' scope",
                                                "The presented API token lacks the 'read' scope. Mint a token carrying it (POST /api/tokens)."),
    ("DELETE", "/api/spaces/sp-1a2b3c4d"): (200, ok({"id": "sp-1a2b3c4d", "status": "Deleting"})),
    ("DELETE", "/api/spaces/sp-gone0001"): NOT_FOUND,
    ("PATCH", "/api/spaces/sp-1a2b3c4d/lease"): (200, ok({"id": "sp-1a2b3c4d", "leaseExpiresAt": "2026-10-05T12:00:00.000Z"})),
    ("PATCH", "/api/spaces/sp-capped01/lease"): refusal(409, "AGENT_KEY_SPEND_CAP", "This agent key spent 1200 of its 1000 cents this month",
                                                         "Delete what the key created, or ask an owner for a key with a higher cap."),
    ("GET", "/api/spaces/sp-1a2b3c4d/kubeconfig/agent"): (200, KUBECONFIG),
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

    def answer(self, status, body, headers=None):
        if isinstance(body, RawJSON):
            raw, ctype = body.encode(), "application/json"
        elif isinstance(body, (dict, list)):
            raw, ctype = json.dumps(body).encode(), "application/json"
        else:
            raw = body.encode()
            ctype = "application/yaml" if body.startswith("apiVersion") else "text/html"
        self.send_response(status)
        for name, value in (headers or {}).items():
            self.send_header(name, value)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def route(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length).decode() if length else ""
        try:
            body = json.loads(raw) if raw and not self.path.startswith("/oauth/") else raw
        except ValueError:
            body = raw
        auth = self.headers.get("Authorization", "")
        if not auth:
            cred = "none"
        elif auth == "Bearer " + TOKEN:
            cred = "minted"
        else:
            cred = "bearer" if auth.startswith("Bearer ") else "other"
        # No Accept header and "*/*" (curl's default) mean the same: the
        # client takes any media type (RFC 9110, 12.5.1). Both log as null.
        accept = self.headers.get("Accept")
        if accept == "*/*":
            accept = None
        with open(LOG, "a", encoding="utf-8") as log:
            log.write(json.dumps({"method": self.command, "path": self.path, "auth": cred,
                                  "accept": accept, "content_type": self.headers.get("Content-Type"),
                                  "body": body}, sort_keys=True) + "\n")
        if self.path == "/oauth/v2/redirect":
            self.send_response(307)
            self.send_header("Location", "/oauth/v2/token")
            self.send_header("Content-Length", "0")
            self.end_headers()
            return None
        if self.path == "/oauth/v2/token":
            return self.answer(200, {"access_token": TOKEN, "token_type": "Bearer", "expires_in": 3600})
        path = self.path.split("?", 1)[0]
        if self.command == "POST" and path == "/api/clusters/register":
            busy = UNAVAILABLE.get(body.get("domain")) if isinstance(body, dict) else None
            if busy:
                status, retry_after, code = busy
                return self.answer(*refusal(status, code, "The registration limiter is not available"),
                                   {"Retry-After": retry_after} if retry_after is not None else None)
            domain = body.get("domain") if isinstance(body, dict) else None
            claim = dict(REGISTERED["claimKey"], name="kubehz-claim-" + str(domain))
            answer = dict(REGISTERED, claimKey=claim)
            if domain in BAD_BIND:
                answer["bindSecret"] = BAD_BIND[domain]
            return self.answer(200, answer)
        if path == "/v1/ssh_keys":
            if self.command == "POST":
                return self.answer(201, {"ssh_key": {"id": 43}})
            name = urllib.parse.parse_qs(urllib.parse.urlsplit(self.path).query).get("name", [""])[0]
            key = SSH_KEYS.get(name)
            return self.answer(200, {"ssh_keys": [key] if key else []})
        if self.command == "DELETE" and path.startswith("/v1/ssh_keys/"):
            return self.answer(200, {})
        if auth != "Bearer " + TOKEN:
            return self.answer(*refusal(401, "UNAUTHORIZED", "Invalid or expired agent key",
                                        "The agent key is revoked or expired. Create a new one under Settings -> Agent keys."))
        if path == "/api/spaces/sp-race0001/kubeconfig/agent":
            with open(os.environ["STUB_RACE_FILE"], "w", encoding="utf-8") as planted:
                planted.write("planted\n")
            return self.answer(200, KUBECONFIG)
        if self.command == "POST" and path.startswith(MAINTENANCE_PREFIX):
            name, _, action = path[len(MAINTENANCE_PREFIX):].partition("/")
            found = MAINTENANCE.get((name, action), refusal(404, "NOT_FOUND", "Node not found in this space",
                                                             "List the nodes with GET /api/spaces/{id}/nodes."))
            if found is DROP:
                self.close_connection = True
                return None
            return self.answer(*found)
        if self.command == "POST" and path == MINT_PATH and isinstance(body, dict):
            return self.answer(*MINTS.get(body.get("nodeName"), refusal(404, "NOT_FOUND", "Cluster not found")))
        if self.command == "POST" and path == JOIN_PATH and isinstance(body, dict):
            ticket = TICKETS.get(body.get("nodeName"))
            if ticket is None:
                return self.answer(*refusal(404, "NOT_FOUND", "Node not found in this space"))
            return self.answer(201, ok({"token": ticket, "expiresAt": "2026-10-04T13:00:00.000Z"}))
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
