package build

// Split the built artifacts.yaml into per-resource files under
// clusters/<domain>/artifacts/ — the committable, GitOps-consumable layout:
//
//	<Kind>.<namespace>.<name>.yaml          namespaced resources
//	<Kind>.<name>.yaml                      cluster-scoped resources
//	Secret[.<namespace>].<name>.sops.yaml   Secrets — NEVER written plaintext;
//	  sops-encrypted (data/stringData only) to the age recipients declared in
//	  spec.gitops.age. No recipients + a Secret in the render = HARD FAIL.
//
// Per-resource files give a per-object git history and are exactly what a
// reconciler (Flux kustomize-controller with native sops decryption) wants
// to watch. artifacts.yaml stays the source `lo deploy` applies — the split
// dir is derived from it AFTER a successful build, never rendered separately.
//
// GitOps shaping applied to the split output ONLY (`lo deploy` semantics
// unchanged):
//   - Jobs lose ttlSecondsAfterFinished: a TTL-reaped Job is "missing" to a
//     reconciler and gets recreated every interval → infinite re-run loop.
//   - Jobs gain kustomize.toolkit.fluxcd.io/force: fixed-name Jobs are
//     immutable, so a spec change must delete+recreate, not patch.
//
// Secret ENCRYPTION is DECOUPLED from the split (spec.build.encrypt,
// resolved by EncryptMode) — splitting shapes every resource; encryption
// governs only the Secret twins:
//   - encrypt.on=change (default): a Secret whose committed twin already
//     DECRYPTS to the same CANONICAL plaintext is KEPT byte-for-byte (no
//     re-encrypt) — sops mints a fresh data key per encrypt, so
//     re-encrypting an unchanged Secret would churn the ciphertext every
//     build. Undecryptable/missing prior ⇒ re-encrypt (fail safe). See
//     secretUnchanged.
//   - encrypt.on=always: re-encrypt every Secret every build (no compare).
//     Pruning is decided by PRESENCE in the render, NOT by whether a Secret
//     was re-encrypted: a kept-because-unchanged Secret survives the swap; a
//     Secret dropped from the render is still pruned.
//
// --no-secrets (LOK8S_BUILD_NO_SECRETS=1) — the CI render path: split ONLY
// non-Secret resources; never render/encrypt/prune/READ a Secret (no
// store/key needed), and the swap's prune EXCLUDES Secret.*.sops.yaml so
// committed encrypted Secrets stay wholly inert.
//
// Ownership rule for stale-file cleanup: this function owns every file in
// artifacts/ whose name starts with an uppercase Kind segment (plus the
// .gitignore guard) — EXCEPT Secret.*.sops.yaml under --no-secrets.
// Lowercase files some envs keep directly under artifacts/
// (kustomization.yaml, capi.yaml, .cache-queue) are never touched.
//
// The YAML stream transforms and the per-Secret fresh renders still EXEC the
// pinned yq with the exact bash expressions: yq's emitter has its own
// opinions (leading `---` on every non-first document, sequence dash
// indentation) that gopkg.in/yaml.v3 does not reproduce byte-for-byte.
// TODO: go native (yaml.Node round-trip) once byte-parity with the pinned yq
// output is proven for the committed domains, not just the parity fixtures.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/ui"
)

var (
	ageKeyRe     = regexp.MustCompile(`^age1[a-z0-9]+$`)
	secretNameRe = regexp.MustCompile(`^[a-z0-9.-]+$`)
	secretNsRe   = regexp.MustCompile(`^[a-z0-9-]+$`)
)

// shapeExpr filters Secrets OUT of the stream and applies the GitOps Job
// shaping — verbatim the bash yq expression.
const shapeExpr = `
      select(.kind != "Secret")
      | (select(.kind == "Job") | .spec) |= del(.ttlSecondsAfterFinished)
      | (select(.kind == "Job") | .metadata.annotations."kustomize.toolkit.fluxcd.io/force") = "enabled"
      | .`

// splitExpr names each document's output file: <Kind>.<namespace>.<name>.yml
// with the namespace segment dropped when empty. The .yml extension is
// appended IN the expression — yq -s only auto-appends when the name has no
// dot, and ours always do. Filename segments come from kustomize-validated
// metadata (RFC 1123), so they are filesystem-safe.
const splitExpr = `([.kind, .metadata.namespace // "", .metadata.name] | filter(. != "") | join(".")) + ".yml"`

// gitignoreContent is the defense net: even a bugged emit must not let a
// plaintext Secret get committed from this dir. Verbatim from bash.
const gitignoreContent = `# GENERATED (build::split). Plaintext Secrets must never be committed —
# only the sops-encrypted twins pass.
Secret.*.yaml
!Secret.*.sops.yaml
`

// secretRef is one Secret document's identity in the render.
type secretRef struct {
	ns, name string
}

// Split splits the built artifacts.yaml per-resource (bash: build::split;
// honors Options.NoSecrets). The steps run in the bash order: the Secret
// policy and recipients (fail closed before any file is written), the
// staged non-Secret emit, the Secret emit, the stage post-conditions, the
// swap.
func Split(ctx context.Context, o Options) error {
	r, err := newSplitRun(o)
	if err != nil {
		return err
	}
	defer r.cleanup()
	if err := r.emitNonSecrets(ctx); err != nil {
		return err
	}
	if err := r.emitSecrets(ctx); err != nil {
		return err
	}
	if err := r.verifyStage(); err != nil {
		return err
	}
	if err := r.swapStage(); err != nil {
		return err
	}
	suffix := ""
	if r.noSecrets {
		suffix = ", --no-secrets: committed Secrets left inert"
	}
	ui.DebugTo(r.stderr, "split: %d file(s) → %s (%d sops Secret(s)%s)", r.emitted, r.outDir, r.secrets, suffix)
	return nil
}

// splitRun is the state of one split: the paths, the Secret policy, the
// scratch dirs and the counters the post-conditions check.
type splitRun struct {
	o         Options
	stderr    io.Writer
	artifact  string
	outDir    string
	noSecrets bool

	docCount          int
	secretRefs        []secretRef
	actualSecretCount int
	// secretCount is the number of Secrets to EMIT: the real count, or 0
	// under --no-secrets.
	secretCount int
	encryptOn   string
	sopsPath    string
	sopsConfig  string
	yqPath      string
	yqOK        bool

	tmpDir string
	stage  string

	emitted int
	secrets int
}

// newSplitRun checks the artifact, resolves the Secret policy and the
// recipients, and creates the scratch dirs (nothing under outDir is
// touched until swapStage).
func newSplitRun(o Options) (*splitRun, error) {
	r := &splitRun{o: o, stderr: o.stderr()}
	domainDir := filepath.Join(o.Paths.Clusters, o.Domain)
	r.artifact = filepath.Join(domainDir, "artifacts.yaml")
	r.outDir = filepath.Join(domainDir, "artifacts")

	if info, err := os.Stat(r.artifact); err != nil || info.Size() == 0 {
		ui.ErrorTo(r.stderr, "no %s to split — build first", r.artifact)
		return nil, ErrHandled
	}

	// --no-secrets: the CI render path. CI has no secrets store and no age
	// key, so it must NOT render, encrypt, prune, or even READ a Secret —
	// yet it must still regenerate the non-Secret artifacts (e.g. after an
	// image-automation pin bump). We therefore treat the render as having
	// ZERO Secrets to emit (secretCount forced to 0 → no recipient check,
	// no sops config, no Secret loop, no secret-count post-condition),
	// while keeping the ACTUAL count for the collision-check arithmetic
	// (non-Secret docs = docCount - actual). Committed Secret.*.sops.yaml
	// files stay wholly inert: never created, never re-encrypted, and —
	// critically — EXCLUDED from the swap's prune (see the guard in
	// swapStage, the single dangerous edge this mode has to defend).
	r.noSecrets = o.NoSecrets
	r.docCount, r.secretRefs = scanArtifact(r.artifact)
	r.actualSecretCount = len(r.secretRefs)
	r.secretCount = r.actualSecretCount
	if r.noSecrets {
		r.secretCount = 0
	}

	// Secret encryption policy (decoupled from the split trigger). Resolved
	// even when there are no Secrets so an unsupported
	// spec.build.encrypt.type still fails the build loudly. In --no-secrets
	// mode it is irrelevant (no Secret is touched) — skip the read so a CI
	// build never needs the encrypt spec valid.
	r.encryptOn = "change"
	if !r.noSecrets {
		_, on, err := EncryptMode(domainDir)
		if err != nil {
			ui.ErrorTo(r.stderr, "%s", err.Error())
			return nil, ErrHandled
		}
		r.encryptOn = on
	}

	// Secrets in the render require declared recipients BEFORE any file is
	// written (fail closed — a plaintext Secret on disk, even briefly, is
	// the incident class this whole mode exists to prevent). Recipient
	// format is validated (bech32 age public keys only — convert SSH keys
	// with ssh-to-age first) before it is interpolated anywhere.
	recipients := ""
	if r.secretCount > 0 {
		if specFile := SpecFile(domainDir); specFile != "" {
			recipients = gitopsAgeRecipients(specFile)
		}
		if recipients == "" {
			ui.ErrorTo(r.stderr, "split: %d Secret(s) in the render but no spec.gitops.age recipients — refusing to write plaintext Secrets. Declare the age public keys (reconciler key + break-glass) in the spec.", r.secretCount)
			return nil, ErrHandled
		}
		for rec := range strings.SplitSeq(recipients, ",") {
			if !ageKeyRe.MatchString(rec) {
				ui.ErrorTo(r.stderr, "split: '%s' is not an age public key (spec.gitops.age)", rec)
				return nil, ErrHandled
			}
		}
		path, ok := execx.Look(o.Paths, "sops")
		if !ok {
			ui.ErrorTo(r.stderr, "split: sops not found (required to encrypt Secrets) — install it (b install) or skip the split for this run with LOK8S_BUILD_SPLIT=0 / lo build --single")
			return nil, ErrHandled
		}
		r.sopsPath = path
	}

	// Everything is assembled in temp dirs first; outDir is only touched in
	// the final swap after every document is emitted, encrypted and
	// verified — a mid-split failure (sops error, collision) must not leave
	// a pruned or partial artifacts/ (the single-file build gets the same
	// guarantee from its tmp+atomic-rename). The stage lives next to outDir
	// so the final moves stay on one filesystem.
	tmpDir, err := os.MkdirTemp("", "tmp.")
	if err != nil {
		ui.ErrorTo(r.stderr, "split: failed to shape %s", r.artifact)
		return nil, ErrHandled
	}
	r.tmpDir = tmpDir
	stage, err := os.MkdirTemp(domainDir, ".artifacts-stage.")
	if err != nil {
		r.cleanup()
		ui.ErrorTo(r.stderr, "split: failed to shape %s", r.artifact)
		return nil, ErrHandled
	}
	r.stage = stage

	// sops discovers a repo-level .sops.yaml from cwd and then REQUIRES a
	// matching creation rule even when recipients are passed — give it a
	// dedicated config (global --config flag; it is not accepted after the
	// subcommand) whose catch-all rule carries the gitops recipients.
	r.sopsConfig = filepath.Join(tmpDir, "sops-gitops.yaml")
	if r.secretCount > 0 {
		content := "creation_rules:\n  - age: '" + recipients + "'\n"
		if err := os.WriteFile(r.sopsConfig, []byte(content), 0o600); err != nil {
			r.cleanup()
			ui.ErrorTo(r.stderr, "split: failed to shape %s", r.artifact)
			return nil, ErrHandled
		}
	}
	r.yqPath, r.yqOK = execx.Look(o.Paths, "yq")
	return r, nil
}

// cleanup drops the scratch dirs (idempotent).
func (r *splitRun) cleanup() {
	if r.tmpDir != "" {
		_ = os.RemoveAll(r.tmpDir)
		r.tmpDir = ""
	}
	if r.stage != "" {
		_ = os.RemoveAll(r.stage)
		r.stage = ""
	}
}

// emitNonSecrets shapes the NON-Secret documents: one yq pass shapes Jobs
// + filters Secrets OUT, a second splits into <Kind>.<namespace>.<name>.yml
// under the tmp dir, then the files move into the stage as .yaml.
func (r *splitRun) emitNonSecrets(ctx context.Context) error {
	streamPath := filepath.Join(r.tmpDir, "nonsecret.stream")
	if !r.yqOK || execToFile(ctx, r.o.runner(), r.yqPath, []string{"eval", shapeExpr, r.artifact}, "", streamPath, r.stderr) != nil {
		ui.ErrorTo(r.stderr, "split: failed to shape %s", r.artifact)
		return ErrHandled
	}
	// Guard the empty stream (a Secrets-only render): yq -s on empty stdin
	// emits one junk '.yml' file for the null document.
	if info, err := os.Stat(streamPath); err == nil && info.Size() > 0 {
		streamFile, err := os.Open(streamPath)
		if err != nil {
			ui.ErrorTo(r.stderr, "split: failed to split %s", r.artifact)
			return ErrHandled
		}
		runErr := r.o.runner().Run(ctx, execx.Cmd{
			Name: r.yqPath, Args: []string{"-s", splitExpr, "-"}, Dir: r.tmpDir,
			Stdin: streamFile, Stdout: io.Discard, Stderr: r.stderr,
		})
		_ = streamFile.Close()
		if runErr != nil {
			ui.ErrorTo(r.stderr, "split: failed to split %s", r.artifact)
			return ErrHandled
		}
		_ = os.Remove(streamPath)
	}

	// Same kind+namespace+name across different API groups would silently
	// overwrite each other in the filename scheme — refuse instead of losing
	// a document (rare; revisit with a group segment if it ever bites).
	// Uses actualSecretCount (not secretCount): the non-Secret stream always
	// excludes EVERY Secret in the render, so the expected non-Secret file
	// count is docCount minus the REAL Secret count — even in --no-secrets
	// mode where secretCount is forced to 0 for the emit path.
	ymlFiles := globSorted(filepath.Join(r.tmpDir, "*.yml"))
	if r.docCount-r.actualSecretCount != len(ymlFiles) {
		ui.ErrorTo(r.stderr, "split: %d non-Secret documents rendered but %d files emitted — kind/namespace/name collision across API groups; not supported", r.docCount-r.actualSecretCount, len(ymlFiles))
		return ErrHandled
	}
	for _, f := range ymlFiles {
		base := filepath.Base(f)
		if err := os.Rename(f, filepath.Join(r.stage, strings.TrimSuffix(base, ".yml")+".yaml")); err != nil {
			ui.ErrorTo(r.stderr, "split: failed to split %s", r.artifact)
			return ErrHandled
		}
		r.emitted++
	}
	return nil
}

// emitSecrets writes every Secret's sops twin into the stage: plaintext
// NEVER touches disk — each is streamed from the artifact straight into
// sops via stdin (--filename-override gives sops its input type hint).
// Selector inputs are guarded even though kustomize already validated them
// (defense against a crafted artifacts.yaml). In --no-secrets mode the
// loop is skipped entirely and no Secret is read, decrypted, or written.
func (r *splitRun) emitSecrets(ctx context.Context) error {
	if !r.noSecrets {
		for _, ref := range r.secretRefs {
			if ref.name == "" {
				continue
			}
			if err := r.emitSecret(ctx, ref); err != nil {
				return err
			}
			r.secrets++
			r.emitted++
		}
	}
	// Count post-condition: a partial Secret listing (the bash process
	// substitution could fail SILENTLY mid-list) would emit fewer Secrets
	// than rendered and the swap below would prune the missing ones from
	// the committed layout (and via a pruning reconciler, from the
	// CLUSTER). Fail instead.
	if r.secrets != r.secretCount {
		ui.ErrorTo(r.stderr, "split: rendered %d Secret(s) but emitted %d — refusing the swap (listing failure?)", r.secretCount, r.secrets)
		return ErrHandled
	}
	return nil
}

// emitSecret stages one Secret's sops twin: kept byte-for-byte when the
// committed twin already decrypts to the fresh render (encrypt.on=change),
// else freshly encrypted; verified to be sops-encrypted either way.
func (r *splitRun) emitSecret(ctx context.Context, ref secretRef) error {
	ns, name := ref.ns, ref.name
	if !secretNameRe.MatchString(name) || (ns != "" && !secretNsRe.MatchString(ns)) {
		ui.ErrorTo(r.stderr, "split: refusing Secret with non-RFC1123 metadata: ns='%s' name='%s'", ns, name)
		return ErrHandled
	}
	nsSeg := ""
	if ns != "" {
		nsSeg = "." + ns
	}
	outfile := filepath.Join(r.stage, "Secret"+nsSeg+"."+name+".sops.yaml")
	prior := filepath.Join(r.outDir, "Secret"+nsSeg+"."+name+".sops.yaml")

	// Capture the fresh render into memory (never a plaintext file).
	// Feeds both the change-detection compare and the encrypt.
	selectExpr := fmt.Sprintf(`select(.kind == "Secret" and .metadata.name == "%s" and (.metadata.namespace // "") == "%s")`, name, ns)
	freshOut, err := execCapture(ctx, r.o.runner(), r.yqPath, []string{"eval", selectExpr, r.artifact}, nil, r.stderr)
	if err != nil {
		ui.ErrorTo(r.stderr, "split: failed to select Secret %s/%s", ns, name)
		return ErrHandled
	}
	// Command-substitution semantics: strip trailing newlines.
	fresh := strings.TrimRight(string(freshOut), "\n")

	// encrypt.on: change — keep the committed twin byte-for-byte when
	// it already decrypts to this canonical plaintext (rationale in
	// secretUnchanged); any decrypt failure / mismatch / missing
	// prior falls through to encrypt.
	if r.encryptOn == "change" && secretUnchanged(ctx, r.o.runner(), r.sopsPath, prior, fresh) {
		if err := copyPreserving(prior, outfile); err != nil {
			ui.ErrorTo(r.stderr, "split: failed to carry forward unchanged Secret %s/%s", ns, name)
			return ErrHandled
		}
		ui.DebugTo(r.stderr, "split: Secret %s/%s unchanged — kept existing ciphertext (encrypt.on=change)", ns, name)
	} else {
		encrypt := execx.Cmd{
			Name: r.sopsPath,
			Args: []string{"--config", r.sopsConfig, "encrypt",
				"--input-type", "yaml", "--output-type", "yaml",
				"--encrypted-regex", `^(data|stringData)$`,
				"--filename-override", "secret.yaml",
				"--output", outfile, "/dev/stdin"},
			Stdin: strings.NewReader(fresh), Stdout: io.Discard, Stderr: r.stderr,
		}
		if err := r.o.runner().Run(ctx, encrypt); err != nil {
			ui.ErrorTo(r.stderr, "split: sops encrypt failed for Secret %s/%s", ns, name)
			return ErrHandled
		}
	}
	// Trust nothing: the file must exist AND carry sops metadata — a
	// masked encrypt failure (exit 0, no/plain output) must not reach
	// the swap.
	if !fileNonEmpty(outfile) || !fileHasLinePrefix(outfile, "sops:") {
		ui.ErrorTo(r.stderr, "split: %s missing or not sops-encrypted — aborting", filepath.Base(outfile))
		return ErrHandled
	}
	return nil
}

// verifyStage checks the post-conditions on the STAGE, before anything
// replaces the live dir: no plaintext Secret escaped, no unrendered
// template residue; then writes the .gitignore guard.
func (r *splitRun) verifyStage() error {
	for _, f := range globSorted(filepath.Join(r.stage, "Secret.*.yaml")) {
		if !strings.HasSuffix(f, ".sops.yaml") {
			ui.ErrorTo(r.stderr, "split: plaintext Secret file(s) present in the staged output — aborting")
			return ErrHandled
		}
	}
	for _, f := range globSorted(filepath.Join(r.stage, "*.yaml")) {
		raw, err := os.ReadFile(f)
		if err == nil && bytes.Contains(raw, []byte("${LOK8S_")) {
			ui.ErrorTo(r.stderr, "split: unrendered ${LOK8S_*} residue in the staged output — check the envsubst whitelist/env")
			return ErrHandled
		}
	}
	if err := os.WriteFile(filepath.Join(r.stage, ".gitignore"), []byte(gitignoreContent), 0o600); err != nil {
		ui.ErrorTo(r.stderr, "split: failed to split %s", r.artifact)
		return ErrHandled
	}
	return nil
}

// swapStage prunes previously generated files (uppercase-Kind ownership
// rule — env-owned lowercase files like kustomization.yaml/capi.yaml
// survive) so objects dropped from the render disappear from git (the
// reconciler's prune signal), then moves the verified stage into place.
//
// --no-secrets GUARD: exclude committed Secret.*.sops.yaml from the
// prune. This mode's stage has NO Secrets (the loop was skipped), so an
// unguarded [A-Z]*.yaml sweep would delete every committed encrypted
// Secret — and a pruning reconciler would then delete them from the
// CLUSTER. The whole point of --no-secrets is that committed Secrets stay
// INERT (never created, re-encrypted, or deleted), so keep them out of
// the sweep. In a normal build secretCount>0 emits fresh Secret twins into
// the stage and they replace the pruned ones, so the (unguarded) sweep
// there is correct.
func (r *splitRun) swapStage() error {
	if err := os.MkdirAll(r.outDir, 0o755); err != nil {
		ui.ErrorTo(r.stderr, "split: failed to split %s", r.artifact)
		return ErrHandled
	}
	for _, name := range generatedFiles(r.outDir) {
		// In --no-secrets mode, never prune an encrypted Secret twin.
		if r.noSecrets && strings.HasSuffix(name, ".sops.yaml") && strings.HasPrefix(name, "Secret.") {
			continue
		}
		_ = os.Remove(filepath.Join(r.outDir, name))
	}
	// bash: `for f in "${stage}"/.gitignore "${stage}"/*` — the shell glob
	// skips dotfiles, so .gitignore is named explicitly.
	moves := []string{filepath.Join(r.stage, ".gitignore")}
	for _, f := range globSorted(filepath.Join(r.stage, "*")) {
		if !strings.HasPrefix(filepath.Base(f), ".") {
			moves = append(moves, f)
		}
	}
	for _, f := range moves {
		if _, err := os.Stat(f); err != nil {
			continue
		}
		if err := os.Rename(f, filepath.Join(r.outDir, filepath.Base(f))); err != nil {
			ui.ErrorTo(r.stderr, "split: failed to split %s", r.artifact)
			return ErrHandled
		}
	}
	return nil
}

// scanArtifact counts the documents in the artifact stream and lists the
// Secret documents' ns/name pairs, in document order. Best-effort like the
// bash `yq … || true` guards: a decode failure yields (0, nil) and the yq
// shape exec fails the split with the same error the bash pipeline printed.
func scanArtifact(artifact string) (int, []secretRef) {
	raw, err := os.ReadFile(artifact)
	if err != nil {
		return 0, nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	docCount := 0
	var refs []secretRef
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name      any `yaml:"name"`
				Namespace any `yaml:"namespace"`
			} `yaml:"metadata"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, nil
		}
		docCount++
		if doc.Kind == "Secret" {
			ns := ""
			if doc.Metadata.Namespace != nil {
				ns = yqToString(doc.Metadata.Namespace)
			}
			refs = append(refs, secretRef{ns: ns, name: yqToString(doc.Metadata.Name)})
		}
	}
	return docCount, refs
}

// secretUnchanged — encrypt.on: change — decides whether a Secret's
// committed sops twin already encodes the freshly-rendered plaintext, so we
// can KEEP it verbatim instead of re-encrypting (sops mints a fresh data key
// on every encrypt, so re-encrypting an identical Secret rewrites the whole
// ciphertext → churns every build's diff).
//
// Returns true ("unchanged", keep the existing file) ONLY when ALL hold:
//   - a prior file exists and is sops-encrypted,
//   - it DECRYPTS here with the ambient age key (same env `lo build` already
//     uses to decrypt the store — the owner key is a recipient of the deploy
//     Secrets),
//   - its decrypted content, CANONICALIZED, equals the fresh render
//     canonicalized.
//
// Any failure (no key, corrupt/missing prior, decrypt error, mismatch)
// returns false → the caller re-encrypts. We can NEVER prove "unchanged"
// without a clean decrypt+match, so every uncertain case falls back to a
// fresh encrypt (safe: at worst an unnecessary re-encrypt, never a stale
// secret shipped).
//
// The compare is CANONICAL: both sides run through the same recursive
// key-sort + block-style remarshal, so key ordering, flow-vs-block style,
// quoting, and trailing-whitespace differences in the rendered YAML never
// masquerade as a change (the whole point is to detect a real VALUE change,
// not a formatting wobble). Decryption is decrypt-to-stdout only — plaintext
// never touches disk (same guarantee as the encrypt path); the fresh render
// stays in memory. Bash: build::_secret_unchanged.
func secretUnchanged(ctx context.Context, r execx.Runner, sopsPath, priorFile, fresh string) bool {
	// No prior, empty, or not sops-encrypted ⇒ cannot be "unchanged".
	if !fileNonEmpty(priorFile) || !fileHasLinePrefix(priorFile, "sops:") {
		return false
	}
	// sops decrypt to stdout (no --output → stays off disk). Any decrypt
	// failure (missing key, wrong recipients, corrupt file) ⇒ fall back to
	// encrypt. Stderr discarded: a decrypt error is EXPECTED in CI-like envs
	// without the key — it's a signal to re-encrypt, not a build error.
	decrypted, err := execCapture(ctx, r, sopsPath, []string{"decrypt", "--input-type", "yaml", "--output-type", "yaml", priorFile}, nil, io.Discard)
	if err != nil || len(decrypted) == 0 {
		return false
	}
	// Canonicalize BOTH sides identically (recursive key sort, forced block
	// style). A parse failure on either side ⇒ treat as changed (re-encrypt).
	canonPrior, err := canonicalYAML(decrypted)
	if err != nil {
		return false
	}
	canonFresh, err := canonicalYAML([]byte(fresh))
	if err != nil {
		return false
	}
	return canonPrior == canonFresh
}

// canonicalYAML re-marshals a YAML stream with every mapping's keys sorted
// recursively and block style forced. Only self-consistency matters: both
// sides of the unchanged-compare run through this same function (the bash
// used `yq -P 'sort_keys(..)'` on both sides for the same reason).
func canonicalYAML(data []byte) (string, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	for {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		sortNode(&node)
		if err := enc.Encode(&node); err != nil {
			return "", err
		}
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// sortNode sorts mapping keys recursively and clears style flags (block
// output).
func sortNode(n *yaml.Node) {
	n.Style = 0
	if n.Kind == yaml.MappingNode {
		type pair struct{ k, v *yaml.Node }
		pairs := make([]pair, 0, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			pairs = append(pairs, pair{n.Content[i], n.Content[i+1]})
		}
		sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].k.Value < pairs[j].k.Value })
		n.Content = n.Content[:0]
		for _, p := range pairs {
			n.Content = append(n.Content, p.k, p.v)
		}
	}
	for _, c := range n.Content {
		sortNode(c)
	}
}

// execCapture runs a command, returning stdout; stderr goes to w.
func execCapture(ctx context.Context, r execx.Runner, path string, args []string, stdin io.Reader, w io.Writer) ([]byte, error) {
	out, err := execx.Output(ctx, r, execx.Cmd{Name: path, Args: args, Stdin: stdin, Stderr: w})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// execToFile runs a command with stdout redirected to outPath.
func execToFile(ctx context.Context, r execx.Runner, path string, args []string, dir, outPath string, stderr io.Writer) error {
	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	runErr := r.Run(ctx, execx.Cmd{
		Name: path, Args: args, Dir: dir,
		Stdin: strings.NewReader(""), Stdout: out, Stderr: stderr,
	})
	// A close error on the written file is a lost write; report it when the
	// command itself succeeded.
	if err := out.Close(); err != nil && runErr == nil {
		return err
	}
	return runErr
}

// copyPreserving copies src to dst keeping mode and timestamps (bash: cp -p).
func copyPreserving(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, raw, info.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}

func fileNonEmpty(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}

// fileHasLinePrefix reports whether any line of the file starts with prefix
// (bash: grep -q '^<prefix>').
func fileHasLinePrefix(path, prefix string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// globSorted matches bash's alphabetically-sorted glob expansion.
func globSorted(pattern string) []string {
	matches, _ := filepath.Glob(pattern)
	sort.Strings(matches)
	return matches
}
