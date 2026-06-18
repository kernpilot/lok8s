package kresource

// Standard k8s Secret types and their required data keys. Used by
// validate.go to enforce that a generated Secret has the keys k8s
// expects for its declared type.
//
// Reference:
// https://kubernetes.io/docs/concepts/configuration/secret/#secret-types

const (
	SecretTypeOpaque               = "Opaque"
	SecretTypeServiceAccountToken  = "kubernetes.io/service-account-token"
	SecretTypeDockerCfg            = "kubernetes.io/dockercfg"
	SecretTypeDockerConfigJSON     = "kubernetes.io/dockerconfigjson"
	SecretTypeBasicAuth            = "kubernetes.io/basic-auth"
	SecretTypeSSHAuth              = "kubernetes.io/ssh-auth"
	SecretTypeTLS                  = "kubernetes.io/tls"
	SecretTypeBootstrapTokenSecret = "bootstrap.kubernetes.io/token"
)

// requiredKeys maps each Secret type to the set of keys k8s requires
// for that type. Opaque has no required keys (anything goes).
var requiredKeys = map[string][]string{
	SecretTypeOpaque:              nil,
	SecretTypeServiceAccountToken: nil, // server-managed
	SecretTypeDockerCfg:           {".dockercfg"},
	SecretTypeDockerConfigJSON:    {".dockerconfigjson"},
	SecretTypeBasicAuth:           {"username", "password"},
	SecretTypeSSHAuth:             {"ssh-privatekey"},
	SecretTypeTLS:                 {"tls.crt", "tls.key"},
	SecretTypeBootstrapTokenSecret: nil, // many optional fields
}

// RequiredKeys returns the required-keys list for the given Secret
// type, or nil if the type is unknown or has no requirements.
func RequiredKeys(secretType string) []string {
	return requiredKeys[secretType]
}

// IsKnownType reports whether secretType is a recognized k8s Secret
// type. Unknown types are still allowed (k8s lets users invent their
// own), but skip required-key validation.
func IsKnownType(secretType string) bool {
	_, ok := requiredKeys[secretType]
	return ok
}
