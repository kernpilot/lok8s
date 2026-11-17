package kubehz

import (
	"errors"
	"strings"
	"testing"
)

// A Hetzner project can be shared: register replaces, and deregister
// deletes, only a claim key that lo made for this cluster, the key with
// the label lok8s.dev/kubehz-claim of the domain. Any other key of the same
// name stays, and lo says why.

// testClaimMark is the label value of test.kubehz.dev, as
// `printf %s test.kubehz.dev | sha256sum | cut -c1-32` prints it. A literal,
// so these cases also run against a tree without the label code.
const testClaimMark = "8c1fa30c717e3eab81f42edf29128ca3"

const (
	claimRegisterAnswer = `{"id":"cl-5","claimKey":{"publicKey":"ssh-ed25519 AAAA k","fingerprint":"aa:bb","name":"kubehz-claim-test.kubehz.dev"}}`
	claimUpload         = `{"name":"kubehz-claim-test.kubehz.dev","public_key":"ssh-ed25519 AAAA k","labels":{"lok8s.dev/kubehz-claim":"` + testClaimMark + `"}}`
	claimOwnLabels      = `{"lok8s.dev/kubehz-claim":"` + testClaimMark + `"}`
	claimOtherLabels    = `{"lok8s.dev/kubehz-claim":"0123456789abcdef0123456789abcdef"}`

	claimLegacyRegister   = "kubehz: lo does not replace the SSH key 'kubehz-claim-test.kubehz.dev' in your Hetzner Cloud project: it has no label lok8s.dev/kubehz-claim, so a person or an older lo made it. If an older lo made it for test.kubehz.dev, delete it (Hetzner Console -> Security -> SSH keys), then run 'lo kubehz register --domain test.kubehz.dev' again."
	claimOtherRegister    = "kubehz: lo does not replace the SSH key 'kubehz-claim-test.kubehz.dev' in your Hetzner Cloud project: its label lok8s.dev/kubehz-claim does not match test.kubehz.dev, so another cluster made it."
	claimLegacyDeregister = "kubehz: lo does not delete the SSH key 'kubehz-claim-test.kubehz.dev' in your Hetzner Cloud project: it has no label lok8s.dev/kubehz-claim, so a person or an older lo made it. If an older lo made it for test.kubehz.dev, delete it by hand (Hetzner Console -> Security -> SSH keys)."
	claimOtherDeregister  = "kubehz: lo does not delete the SSH key 'kubehz-claim-test.kubehz.dev' in your Hetzner Cloud project: its label lok8s.dev/kubehz-claim does not match test.kubehz.dev, so another cluster made it."
)

// claimKey is one key of a GET /v1/ssh_keys?name= answer, with the given
// id and labels JSON ("" leaves the field out).
func claimKey(id, labels string) string {
	k := `"name":"kubehz-claim-test.kubehz.dev"`
	if id != "" {
		k = `"id":` + id + `,` + k
	}
	if labels != "" {
		k += `,"labels":` + labels
	}
	return "{" + k + "}"
}

// claimKeys is a GET /v1/ssh_keys?name= answer with the key 42 and the
// given labels JSON ("" leaves the labels field out).
func claimKeys(labels string) string { return `{"ssh_keys":[` + claimKey("42", labels) + `]}` }

// claimKeyCase is one hcloud answer and what lo must do with it.
type claimKeyCase struct {
	name    string
	status  int    // the status of the GET; 0 = 200
	keys    string // the GET /v1/ssh_keys?name= answer
	deleted string // the key id lo deletes; "" = none
	warn    string // the refusal; "" = none
}

var claimKeyCases = []claimKeyCase{
	{"own key", 0, claimKeys(claimOwnLabels), "42", ""},
	{"no key", 0, `{"ssh_keys":[]}`, "", ""},
	{"legacy key without labels", 0, claimKeys(""), "", "legacy"},
	{"key with empty labels", 0, claimKeys(`{}`), "", "legacy"},
	{"foreign key with another label", 0, claimKeys(`{"owner":"alice"}`), "", "legacy"},
	{"key of another cluster", 0, claimKeys(claimOtherLabels), "", "other"},
	// Nothing to act on: lo deletes nothing. register uploads: a key of
	// that name that the list did not show makes the upload fail, and
	// register falls back.
	{"list fails", 500, `{"error":{"code":"server_error"}}`, "", ""},
	{"answer does not parse", 0, `<html>`, "", ""},
	{"key without an id", 0, `{"ssh_keys":[` + claimKey("", claimOwnLabels) + `]}`, "", ""},
	// Only the first key counts. A foreign first key is never deleted, and
	// neither is the key after it.
	{"two keys, foreign first", 0, `{"ssh_keys":[` + claimKey("42", "") + `,` + claimKey("43", claimOwnLabels) + `]}`, "", "legacy"},
	{"two keys, own first", 0, `{"ssh_keys":[` + claimKey("42", claimOwnLabels) + `,` + claimKey("43", "") + `]}`, "42", ""},
}

// claimKeyDeletes is the ids of the hcloud key DELETEs, in order.
func claimKeyDeletes(h *harness) string {
	ids := ""
	for _, r := range h.reqs() {
		if r.Method == "DELETE" && strings.HasPrefix(r.Path, "/v1/ssh_keys/") {
			ids += strings.TrimPrefix(r.Path, "/v1/ssh_keys/")
		}
	}
	return ids
}

// serveClaimKeys answers the key list of tc and every key DELETE.
func serveClaimKeys(h *harness, tc claimKeyCase) {
	status := tc.status
	if status == 0 {
		status = 200
	}
	h.handle("GET /v1/ssh_keys", status, tc.keys)
	h.handle("DELETE /v1/ssh_keys/42", 204, ``)
	h.handle("DELETE /v1/ssh_keys/43", 204, ``)
}

func TestClaimKeyOwnershipRegister(t *testing.T) {
	t.Parallel()
	for _, tc := range claimKeyCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.env["HCLOUD_TOKEN"], h.env["HCLOUD_API_BASE"] = "hc_test", h.apiURL()
			h.handle("POST /api/clusters/register", 200, claimRegisterAnswer)
			serveClaimKeys(h, tc)
			h.handle("POST /v1/ssh_keys", 201, `{"ssh_key":{"id":43}}`)
			err := h.ctx.ensureClaimKey(t.Context(), "test.kubehz.dev", h.apiURL())
			upload := h.lastReq("POST", "/v1/ssh_keys")
			if r := h.lastReq("GET", "/v1/ssh_keys"); r == nil || r.Query != "name=kubehz-claim-test.kubehz.dev" {
				t.Fatalf("key lookup: %+v", r)
			}
			if got := claimKeyDeletes(h); got != tc.deleted {
				t.Errorf("deleted keys = %q, want %q", got, tc.deleted)
			}
			switch tc.warn {
			case "":
				// Own key or none: the new key goes up with the label, after
				// the old one is gone.
				mustOK(t, err, h.output())
				if upload == nil || upload.Body != claimUpload {
					t.Fatalf("upload = %+v, want the body %s", upload, claimUpload)
				}
				if tc.deleted != "" {
					order := ""
					for _, r := range h.reqs() {
						if r.Method == "DELETE" || r.Method == "POST" && r.Path == "/v1/ssh_keys" {
							order += r.Method + " "
						}
					}
					if order != "DELETE POST " {
						t.Errorf("order = %q, want the DELETE before the POST", order)
					}
				}
				mustNotContain(t, h.output(), "lo does not replace")
			case "legacy", "other":
				mustErr(t, err)
				if upload != nil {
					t.Errorf("upload sent next to a key lo did not make: %+v", upload)
				}
				want := map[string]string{"legacy": claimLegacyRegister, "other": claimOtherRegister}[tc.warn]
				mustContain(t, h.output(), want)
			}
		})
	}
}

// A key of the claim key's name that lo did not make for this cluster: the
// register step stops with errClaimKeyNotOwn itself (errors.Is), not with
// any other error, and sends no DELETE and no upload for it.
func TestEnsureClaimKeyForeignKeyIsNotOwn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, labels string }{
		{"key without labels", ""},
		{"key of another cluster", claimOtherLabels},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.env["HCLOUD_TOKEN"], h.env["HCLOUD_API_BASE"] = "hc_test", h.apiURL()
			h.handle("POST /api/clusters/register", 200, claimRegisterAnswer)
			h.handle("GET /v1/ssh_keys", 200, claimKeys(tc.labels))
			h.handle("DELETE /v1/ssh_keys/42", 204, ``)
			h.handle("POST /v1/ssh_keys", 201, `{"ssh_key":{"id":43}}`)
			err := h.ctx.ensureClaimKey(t.Context(), "test.kubehz.dev", h.apiURL())
			if !errors.Is(err, errClaimKeyNotOwn) {
				t.Fatalf("err = %v, want errClaimKeyNotOwn", err)
			}
			if r := h.lastReq("GET", "/v1/ssh_keys"); r == nil || r.Query != "name=kubehz-claim-test.kubehz.dev" {
				t.Fatalf("key lookup: %+v", r)
			}
			if h.anyReq("DELETE", "/v1/ssh_keys") {
				t.Errorf("DELETE sent for a key lo did not make: %+v", h.reqs())
			}
			if r := h.lastReq("POST", "/v1/ssh_keys"); r != nil {
				t.Errorf("upload sent next to a key lo did not make: %+v", r)
			}
		})
	}
}

func TestClaimKeyOwnershipDeregister(t *testing.T) {
	t.Parallel()
	for _, tc := range claimKeyCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.env["HCLOUD_TOKEN"], h.env["HCLOUD_API_BASE"] = "hc_test", h.apiURL()
			h.handle("GET /api/clusters", 200, registryListFixture)
			h.handle("DELETE /api/clusters/cl-old", 200, `{"ok":true}`)
			serveClaimKeys(h, tc)
			// The key step is best-effort: a refused key leaves the result
			// of the platform removal as it is.
			mustOK(t, h.ctx.DeregisterCluster(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", ""), h.output())
			if r := h.lastReq("GET", "/v1/ssh_keys"); r == nil || r.Query != "name=kubehz-claim-test.kubehz.dev" {
				t.Fatalf("key lookup: %+v", r)
			}
			if got := claimKeyDeletes(h); got != tc.deleted {
				t.Errorf("deleted keys = %q, want %q", got, tc.deleted)
			}
			switch tc.warn {
			case "":
				mustNotContain(t, h.output(), "lo does not delete")
			case "legacy":
				mustContain(t, h.output(), claimLegacyDeregister)
			case "other":
				mustContain(t, h.output(), claimOtherDeregister)
			}
		})
	}
}
