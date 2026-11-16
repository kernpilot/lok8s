package kubehz

import "testing"

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

// claimKeys is a GET /v1/ssh_keys?name= answer with the key 42 and the
// given labels JSON ("" leaves the labels field out).
func claimKeys(labels string) string {
	if labels == "" {
		return `{"ssh_keys":[{"id":42,"name":"kubehz-claim-test.kubehz.dev"}]}`
	}
	return `{"ssh_keys":[{"id":42,"name":"kubehz-claim-test.kubehz.dev","labels":` + labels + `}]}`
}

// claimKeyCase is one hcloud answer and what lo must do with it.
type claimKeyCase struct {
	name   string
	keys   string // the GET /v1/ssh_keys?name= answer
	delete bool   // DELETE /v1/ssh_keys/42
	warn   string // the refusal; "" = none
}

var claimKeyCases = []claimKeyCase{
	{"own key", claimKeys(claimOwnLabels), true, ""},
	{"no key", `{"ssh_keys":[]}`, false, ""},
	{"legacy key without labels", claimKeys(""), false, "legacy"},
	{"key with empty labels", claimKeys(`{}`), false, "legacy"},
	{"foreign key with another label", claimKeys(`{"owner":"alice"}`), false, "legacy"},
	{"key of another cluster", claimKeys(claimOtherLabels), false, "other"},
}

func TestClaimKeyOwnershipRegister(t *testing.T) {
	t.Parallel()
	for _, tc := range claimKeyCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.env["HCLOUD_TOKEN"], h.env["HCLOUD_API_BASE"] = "hc_test", h.apiURL()
			h.handle("POST /api/clusters/register", 200, claimRegisterAnswer)
			h.handle("GET /v1/ssh_keys", 200, tc.keys)
			h.handle("DELETE /v1/ssh_keys/42", 204, ``)
			h.handle("POST /v1/ssh_keys", 201, `{"ssh_key":{"id":43}}`)
			err := h.ctx.ensureClaimKey(t.Context(), "test.kubehz.dev", h.apiURL())
			upload := h.lastReq("POST", "/v1/ssh_keys")
			if got := h.anyReq("DELETE", "/v1/ssh_keys/42"); got != tc.delete {
				t.Errorf("DELETE of the key sent = %v, want %v", got, tc.delete)
			}
			switch tc.warn {
			case "":
				// Own key or none: the new key goes up with the label, after
				// the old one is gone.
				mustOK(t, err, h.output())
				if upload == nil || upload.Body != claimUpload {
					t.Fatalf("upload = %+v, want the body %s", upload, claimUpload)
				}
				if tc.delete {
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

func TestClaimKeyOwnershipDeregister(t *testing.T) {
	t.Parallel()
	for _, tc := range claimKeyCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.env["HCLOUD_TOKEN"], h.env["HCLOUD_API_BASE"] = "hc_test", h.apiURL()
			h.handle("GET /api/clusters", 200, registryListFixture)
			h.handle("DELETE /api/clusters/cl-old", 200, `{"ok":true}`)
			h.handle("GET /v1/ssh_keys", 200, tc.keys)
			h.handle("DELETE /v1/ssh_keys/42", 204, ``)
			// The key step is best-effort: a refused key leaves the result
			// of the platform removal as it is.
			mustOK(t, h.ctx.DeregisterCluster(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", ""), h.output())
			if r := h.lastReq("GET", "/v1/ssh_keys"); r == nil || r.Query != "name=kubehz-claim-test.kubehz.dev" {
				t.Fatalf("key lookup: %+v", r)
			}
			if got := h.anyReq("DELETE", "/v1/ssh_keys/42"); got != tc.delete {
				t.Errorf("DELETE of the key sent = %v, want %v", got, tc.delete)
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
