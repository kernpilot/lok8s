// Package build is the Go port of the argsh build pipeline
// (.lok8s/libs/build): kustomize render of a domain's composed kustomization
// into one clusters/<domain>/artifacts.yaml, plus the spec-declared split
// emit into per-resource GitOps files under clusters/<domain>/artifacts/.
//
// `kustomize build` stays an external exec (renderer-version drift is a
// measured incident class); the yq spec reads and the envsubst stage are
// native. The split-mode YAML stream transforms still exec the pinned yq —
// see split.go for why.
package build

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// specDoc is the subset of a cluster/deploy spec the build pipeline reads.
type specDoc struct {
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Build struct {
			Artifacts string `yaml:"artifacts"`
			Encrypt   struct {
				Type string `yaml:"type"`
				On   string `yaml:"on"`
			} `yaml:"encrypt"`
		} `yaml:"build"`
		Gitops struct {
			Provider string `yaml:"provider"`
			Age      []any  `yaml:"age"`
		} `yaml:"gitops"`
		ClusterRef struct {
			Domain string `yaml:"domain"`
		} `yaml:"clusterRef"`
	} `yaml:"spec"`
}

// readSpec parses a spec file best-effort: a missing/unreadable/unparsable
// file yields the zero value, matching the bash `yq -r '… // ""' || val=""`
// guards which never fail the build over a bad spec read.
func readSpec(specFile string) specDoc {
	var doc specDoc
	raw, err := os.ReadFile(specFile)
	if err != nil {
		return specDoc{}
	}
	if yaml.Unmarshal(raw, &doc) != nil {
		return specDoc{}
	}
	return doc
}

// SpecFile resolves the domain's spec file: cluster domains carry
// cluster.lok8s.yaml, deploy domains deploy.lok8s.yaml. Returns the path
// (empty when neither exists). Bash: build::_spec_file.
func SpecFile(domainDir string) string {
	for _, name := range []string{"cluster.lok8s.yaml", "deploy.lok8s.yaml"} {
		specFile := filepath.Join(domainDir, name)
		if info, err := os.Stat(specFile); err == nil && !info.IsDir() {
			return specFile
		}
	}
	return ""
}

// ArtifactsMode resolves the domain's artifacts mode: "split" or "single"
// (default). Declared via spec.build.artifacts; setting a
// spec.gitops.provider IMPLIES split (a GitOps consumer without committable
// per-resource output is meaningless) unless the spec explicitly pins
// artifacts: single. Bash: build::_artifacts_mode.
func ArtifactsMode(domainDir string) string {
	specFile := SpecFile(domainDir)
	if specFile != "" {
		doc := readSpec(specFile)
		declared := doc.Spec.Build.Artifacts
		provider := doc.Spec.Gitops.Provider
		if declared == "split" || (provider != "" && declared != "single") {
			return "split"
		}
	}
	return "single"
}

// EncryptMode resolves the domain's Secret ENCRYPTION policy for split mode —
// DECOUPLED from the split trigger (splitting shapes every resource;
// encryption governs only the Secret twins). Declared via spec.build.encrypt:
//
//	spec.build.encrypt.type   how Secrets are encrypted. Default (and only
//	                          value supported today) is `sops`. Anything else
//	                          is a hard error — silently ignoring an unknown
//	                          backend would ship plaintext-intent under a
//	                          typo'd key.
//	spec.build.encrypt.on     WHEN a Secret is re-encrypted:
//	                            change  (default) re-encrypt only when the
//	                                    decrypted prior differs from the
//	                                    fresh render — kills the per-build
//	                                    ciphertext churn sops's
//	                                    fresh-per-encrypt data key would
//	                                    otherwise inflict (see split.go).
//	                            always  re-encrypt every Secret every build
//	                                    (the original behavior; no
//	                                    decrypt/compare).
//
// An absent `encrypt:` block yields the defaults {type: sops, on: change};
// an absent key AND an explicit empty string both fall back to the default.
// Bash: build::_encrypt_mode.
func EncryptMode(domainDir string) (encType, encOn string, err error) {
	encType, encOn = "sops", "change"
	if specFile := SpecFile(domainDir); specFile != "" {
		doc := readSpec(specFile)
		if doc.Spec.Build.Encrypt.Type != "" {
			encType = doc.Spec.Build.Encrypt.Type
		}
		if doc.Spec.Build.Encrypt.On != "" {
			encOn = doc.Spec.Build.Encrypt.On
		}
	}
	if encType != "sops" {
		return "", "", fmt.Errorf("spec.build.encrypt.type '%s' is not supported (only 'sops')", encType)
	}
	if encOn != "change" && encOn != "always" {
		return "", "", fmt.Errorf("spec.build.encrypt.on '%s' is invalid (use 'change' or 'always')", encOn)
	}
	return encType, encOn, nil
}

// specMetadataName reads .metadata.name from a spec file, "" when missing or
// unreadable (bash: yq -r '.metadata.name // ""').
func specMetadataName(specFile string) string {
	return readSpec(specFile).Metadata.Name
}

// gitopsAgeRecipients reads spec.gitops.age as a comma-joined string (bash:
// yq -r '(.spec.gitops.age // []) | join(",")').
func gitopsAgeRecipients(specFile string) string {
	entries := readSpec(specFile).Spec.Gitops.Age
	out := ""
	for i, e := range entries {
		if i > 0 {
			out += ","
		}
		out += yqToString(e)
	}
	return out
}

// yqToString mirrors yq's `tostring` scalar rendering for the values the
// build pipeline interpolates (null → "null", bools/numbers verbatim).
func yqToString(v any) string {
	if v == nil {
		return "null"
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// hasKustomization checks if a directory has a kustomization file
// (bash: _has_kustomization).
func hasKustomization(dir string) bool {
	for _, name := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}

// NoSecretsEffective resolves the effective --no-secrets bit. The FLAG wins
// when on; otherwise a pre-set LOK8S_BUILD_NO_SECRETS env (the documented
// equivalent trigger, `LOK8S_BUILD_NO_SECRETS=1 lo build`) is honored rather
// than clobbered. Presence-only: there is no `--no-secrets=false` off-form in
// the bash CLI (argsh strips `=…` and forces the value to 1), so "flag wins"
// means "flag ON wins". Bash: build::_no_secrets.
func NoSecretsEffective(flag bool) bool {
	return flag || os.Getenv("LOK8S_BUILD_NO_SECRETS") == "1"
}
