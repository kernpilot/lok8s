package lo

// ip.go — pure IP arithmetic and validation (.lok8s/utils/ip.sh). No side
// effects, no dependencies beyond integer arithmetic; error strings are the
// exact raw `error: …` family the bash printed to stderr.

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

var ipv4Re = regexp.MustCompile(`^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$`)

// ipValidateFormat validates dotted-quad IPv4 format (0-255 per octet),
// printing the bash-exact error to errOut (bash: ip::validate_format).
func ipValidateFormat(ip string, errOut io.Writer) bool {
	m := ipv4Re.FindStringSubmatch(ip)
	if m == nil {
		fmt.Fprintf(errOut, "error: invalid IP format '%s'\n", ip)
		return false
	}
	for _, oct := range m[1:] {
		n, _ := strconv.Atoi(oct)
		if n > 255 {
			fmt.Fprintf(errOut, "error: IP octet out of range in '%s'\n", ip)
			return false
		}
	}
	return true
}

// ipToInt converts a dotted quad to a 32-bit integer (bash: ip::to_int).
// The bool is false for a malformed IP (nothing is printed — callers that
// want the format error use ipValidateFormat first, matching the bash
// 2>/dev/null suppression at the tolerant call sites).
func ipToInt(ip string) (uint32, bool) {
	m := ipv4Re.FindStringSubmatch(ip)
	if m == nil {
		return 0, false
	}
	var n uint32
	for _, oct := range m[1:] {
		v, _ := strconv.Atoi(oct)
		if v > 255 {
			return 0, false
		}
		n = n<<8 + uint32(v) // #nosec G115 -- the regex admits digits only; 0..255 checked above
	}
	return n, true
}

// ipFromInt converts a 32-bit integer back to a dotted quad (bash:
// ip::from_int).
func ipFromInt(n uint32) string {
	return fmt.Sprintf("%d.%d.%d.%d", n>>24&255, n>>16&255, n>>8&255, n&255)
}

// ipAdd adds an offset to an IP address (bash: ip::add).
func ipAdd(ip string, offset int) (string, bool) {
	n, ok := ipToInt(ip)
	if !ok {
		return "", false
	}
	// #nosec G115 -- a negative offset wraps to the modular add the bash
	// arithmetic performs; the result is masked to four octets.
	return ipFromInt(n + uint32(offset)), true
}

// ipInSubnet reports whether ip lies within cidr — INCLUSIVE of the network
// and broadcast addresses, exactly like the bash arithmetic (bash:
// ip::validate_in_subnet).
func ipInSubnet(ip, cidr string) bool {
	subnetIP, prefixStr, ok := strings.Cut(cidr, "/")
	if !ok {
		return false
	}
	prefix, err := strconv.Atoi(prefixStr)
	if err != nil || prefix < 0 || prefix > 32 {
		return false
	}
	ipInt, ok := ipToInt(ip)
	if !ok {
		return false
	}
	subnetInt, ok := ipToInt(subnetIP)
	if !ok {
		return false
	}
	mask := uint32(0xFFFFFFFF) << (32 - prefix) & 0xFFFFFFFF
	if prefix == 0 {
		mask = 0
	}
	network := subnetInt & mask
	broadcast := network | (^mask & 0xFFFFFFFF)
	return ipInt >= network && ipInt <= broadcast
}
