package addons

// suc.go — the Kubernetes minors that the kubectl image of the
// system-upgrade-controller addon serves (bash: spec::suc_kubectl in
// .lok8s/utils/spec.sh, the same rules and lines).
//
// For a Plan with cordon or drain, the controller adds a cordon and a drain
// init container to the Plan's Job. Both run the kubectl image that the
// addon pins (kubectl.image in .lok8s/addons/system-upgrade-controller/
// values.yaml). rancher/kuberlr-kubectl v8.1.1 bundles kubectl v1.34.8,
// v1.35.5 and v1.36.1, and its binary downloads are off. kuberlr accepts one
// minor of skew, so the image serves Kubernetes 1.33 to 1.37 only. On
// another minor the drain container fails at once, the Job retries, and the
// Plan never completes. A Plan without cordon and drain is not affected.
//
// `lo doctor` and `lo lint` compare spec.kubernetes.version with the range.
// TestSUCKubectlRangeMatchesThePin fails when the tag of the pin moves and
// the range does not.

import (
	"fmt"
	"regexp"
	"strconv"
)

const (
	// SUCAddon is the entry name of the addon in spec.bootstrap.
	SUCAddon = "system-upgrade-controller"
	// SUCKubectlMinMinor and SUCKubectlMaxMinor are the first and the last
	// Kubernetes 1.x minor that the pinned kubectl image serves.
	SUCKubectlMinMinor = 33
	SUCKubectlMaxMinor = 37
)

// SUCKubectlState is the verdict for one value of spec.kubernetes.version.
type SUCKubectlState int

const (
	// SUCKubectlInRange: the image serves the minor.
	SUCKubectlInRange SUCKubectlState = iota
	// SUCKubectlOutOfRange: the image does not serve the minor.
	SUCKubectlOutOfRange
	// SUCKubectlNotAVersion: the value is not vX.Y or vX.Y.Z (with an
	// optional @sha256 digest), so there is no minor to compare.
	SUCKubectlNotAVersion
)

// sucVersionRe is vX.Y or vX.Y.Z, the v optional, with an optional @sha256
// digest (a Lo version can carry one). Major and minor have no leading zero
// and at most three digits (the bash twin compares them as decimal numbers).
var sucVersionRe = regexp.MustCompile(`^v?(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})(\.[0-9]+)?(@sha256:[0-9a-f]{64})?$`)

// SUCKubectl compares a set value of spec.kubernetes.version with the range.
// It returns the verdict and the minor as "major.minor" (empty for
// SUCKubectlNotAVersion).
func SUCKubectl(version string) (SUCKubectlState, string) {
	m := sucVersionRe.FindStringSubmatch(version)
	if m == nil {
		return SUCKubectlNotAVersion, ""
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	shown := m[1] + "." + m[2]
	if major != 1 || minor < SUCKubectlMinMinor || minor > SUCKubectlMaxMinor {
		return SUCKubectlOutOfRange, shown
	}
	return SUCKubectlInRange, shown
}

// SUCKubectlRange is the range as the lines name it: "1.33 to 1.37".
func SUCKubectlRange() string {
	return fmt.Sprintf("1.%d to 1.%d", SUCKubectlMinMinor, SUCKubectlMaxMinor)
}

// SUCKubectlOutOfRangeLine is the warning of `lo doctor` and `lo lint` for
// a minor that the image does not serve.
func SUCKubectlOutOfRangeLine(minor string) string {
	r := SUCKubectlRange()
	return "system-upgrade-controller: the kubectl image of the drain container serves Kubernetes " + r +
		", not " + minor + ", so a Plan with cordon or drain fails: set spec.kubernetes.version to a version from " + r +
		", or use a Plan without cordon or drain"
}
