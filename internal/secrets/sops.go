package secrets

// SOPS binary-mode encrypt/decrypt via the sops library (the bash
// implementation execs the sops binary with `--input-type binary
// --output-type binary`; the JSON binary store below is exactly what those
// flags select, so the produced .enc files stay interoperable with the sops
// CLI — encryption is nondeterministic, byte parity is not a goal, cross-tool
// decrypt is).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	sops "github.com/getsops/sops/v3"
	"github.com/getsops/sops/v3/aes"
	"github.com/getsops/sops/v3/age"
	"github.com/getsops/sops/v3/cmd/sops/common"
	sopsconfig "github.com/getsops/sops/v3/config"
	"github.com/getsops/sops/v3/decrypt"
	"github.com/getsops/sops/v3/keyservice"
	sopsjson "github.com/getsops/sops/v3/stores/json"
	"github.com/getsops/sops/v3/version"
)

// binaryStore is the store the sops CLI uses for --input-type/--output-type
// binary: raw bytes in a JSON envelope.
func binaryStore() *sopsjson.BinaryStore {
	return sopsjson.NewBinaryStore(&sopsconfig.JSONBinaryStoreConfig{})
}

// sopsEncryptFile encrypts plaintextPath to encPath.
//
// The explicit config path is load-bearing: the bash implementation pins
// `sops --config "${sops_config}"` so the SAME .sops.yaml the caller's gate
// just checked supplies the creation rules — without it sops discovers config
// by walking up from the FILE's path, so a different .sops.yaml higher up
// (e.g. the repo root, or under Docker) could silently supply other
// recipients than the one validated. LoadCreationRuleForFile applies the
// CLI's own first-match-wins path_regex logic (matched against the file path
// relative to the config file's directory).
func sopsEncryptFile(configPath, plaintextPath, encPath string) error {
	fileBytes, err := os.ReadFile(plaintextPath)
	if err != nil {
		return err
	}
	absPlain, err := filepath.Abs(plaintextPath)
	if err != nil {
		return err
	}
	cfg, err := sopsconfig.LoadCreationRuleForFile(configPath, absPlain, nil)
	if err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("error loading config: no matching creation rules found")
	}

	store := binaryStore()
	branches, err := store.LoadPlainFile(fileBytes)
	if err != nil {
		return err
	}
	tree := sops.Tree{
		Branches: branches,
		Metadata: sops.Metadata{
			KeyGroups:               cfg.KeyGroups,
			UnencryptedSuffix:       cfg.UnencryptedSuffix,
			EncryptedSuffix:         cfg.EncryptedSuffix,
			UnencryptedRegex:        cfg.UnencryptedRegex,
			EncryptedRegex:          cfg.EncryptedRegex,
			UnencryptedCommentRegex: cfg.UnencryptedCommentRegex,
			EncryptedCommentRegex:   cfg.EncryptedCommentRegex,
			MACOnlyEncrypted:        cfg.MACOnlyEncrypted,
			Version:                 version.Version,
			ShamirThreshold:         cfg.ShamirThreshold,
		},
		FilePath: absPlain,
	}
	dataKey, errs := tree.GenerateDataKeyWithKeyServices(
		[]keyservice.KeyServiceClient{keyservice.NewLocalClient()},
	)
	if len(errs) > 0 {
		return fmt.Errorf("could not generate data key: %s", errs)
	}
	if err := common.EncryptTree(common.EncryptTreeOpts{
		DataKey: dataKey,
		Tree:    &tree,
		Cipher:  aes.NewCipher(),
	}); err != nil {
		return err
	}
	out, err := store.EmitEncryptedFile(tree)
	if err != nil {
		return err
	}
	if err := os.WriteFile(encPath, out, 0o600); err != nil {
		return err
	}
	nudgeNewer(encPath, plaintextPath)
	return nil
}

// DecryptYAMLFile decrypts a sops-encrypted YAML file (a
// restore.d/*.sops.yaml manifest) IN MEMORY and returns the plaintext. The
// counterpart of the bash `sops -d "${f}"` in bootstrap::_restore_d — key
// discovery is ambient (SOPS_AGE_KEY / SOPS_AGE_KEY_FILE / the age keys
// dir), exactly like the CLI. Capturing the plaintext in memory (instead of
// a sops|kubectl pipe) is deliberate: piping sops's 2>&1 into kubectl once
// corrupted the YAML stream with sops warnings — see the bash comment at
// the bootstrap::_restore_d call site.
func DecryptYAMLFile(path string) ([]byte, error) {
	return decrypt.File(path, "yaml")
}

// ageDecryptor is a local sops key service that decrypts age-wrapped data
// keys with identities handed to it directly — the library's own channel
// for injected identities (age.ParsedIdentities.ApplyToMasterKey), which
// keeps the private key out of the process environment. The bash
// implementation passed it as `SOPS_AGE_KEY=… sops decrypt …`; the stock
// key service would read that same variable, so every other key type
// falls through to it untouched.
type ageDecryptor struct {
	keyservice.Server
	identities age.ParsedIdentities
}

func (d ageDecryptor) Decrypt(ctx context.Context, req *keyservice.DecryptRequest) (*keyservice.DecryptResponse, error) {
	k, ok := req.Key.KeyType.(*keyservice.Key_AgeKey)
	if !ok {
		return d.Server.Decrypt(ctx, req)
	}
	key := age.MasterKey{Recipient: k.AgeKey.Recipient, EncryptedKey: string(req.Ciphertext)}
	d.identities.ApplyToMasterKey(&key)
	plaintext, err := key.Decrypt()
	if err != nil {
		return nil, err
	}
	return &keyservice.DecryptResponse{Plaintext: plaintext}, nil
}

// sopsDecryptData decrypts a binary-mode .enc payload with the given age
// identity (decrypt.Data with the identity injected instead of read from
// SOPS_AGE_KEY). Decrypt does NOT consult .sops.yaml (recipients live in
// the file's own metadata).
func sopsDecryptData(encBytes []byte, ageKey string) ([]byte, error) {
	var ids age.ParsedIdentities
	if err := ids.Import(ageKey); err != nil {
		return nil, err
	}
	store := binaryStore()
	tree, err := store.LoadEncryptedFile(encBytes)
	if err != nil {
		return nil, err
	}
	svc := keyservice.NewCustomLocalClient(ageDecryptor{identities: ids})
	dataKey, err := tree.Metadata.GetDataKeyWithKeyServices([]keyservice.KeyServiceClient{svc}, nil)
	if err != nil {
		return nil, err
	}
	cipher := aes.NewCipher()
	mac, err := tree.Decrypt(dataKey, cipher)
	if err != nil {
		return nil, err
	}
	originalMac, err := cipher.Decrypt(tree.Metadata.MessageAuthenticationCode, dataKey, tree.Metadata.LastModified.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt original mac: %w", err)
	}
	if originalMac != mac {
		return nil, fmt.Errorf("failed to verify data integrity. expected mac %q, got %q", originalMac, mac)
	}
	return store.EmitPlainFile(tree.Branches)
}
