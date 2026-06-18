# shellcheck shell=bash
# ip.sh — Pure IP arithmetic and validation utilities
# No side effects, no external dependencies beyond bash arithmetic.

# Validate dotted-quad IPv4 format (0-255 per octet)
# Usage: ip::validate_format <ip>
ip::validate_format() {
  local ip="$1"
  if [[ ! "${ip}" =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$ ]]; then
    echo "error: invalid IP format '${ip}'" >&2
    return 1
  fi
  local a="${BASH_REMATCH[1]}" b="${BASH_REMATCH[2]}" c="${BASH_REMATCH[3]}" d="${BASH_REMATCH[4]}"
  if (( a > 255 || b > 255 || c > 255 || d > 255 )); then
    echo "error: IP octet out of range in '${ip}'" >&2
    return 1
  fi
}

# Convert dotted-quad IP to a 32-bit integer
# Usage: ip::to_int <ip>
ip::to_int() {
  local ip="$1"
  ip::validate_format "${ip}" || return 1
  local a b c d
  IFS='.' read -r a b c d <<< "${ip}"
  echo $(( (a << 24) + (b << 16) + (c << 8) + d ))
}

# Convert 32-bit integer back to dotted-quad IP
# Usage: ip::from_int <int>
ip::from_int() {
  local n="$1"
  echo "$(( (n >> 24) & 255 )).$(( (n >> 16) & 255 )).$(( (n >> 8) & 255 )).$(( n & 255 ))"
}

# Add an offset to an IP address
# Usage: ip::add <ip> <offset>
ip::add() {
  local ip="$1" offset="$2"
  local n
  n=$(ip::to_int "${ip}")
  ip::from_int $(( n + offset ))
}

# Validate that an IP is within a CIDR subnet
# Usage: ip::validate_in_subnet <ip> <cidr>
# Returns 0 if valid, 1 if not
ip::validate_in_subnet() {
  local ip="$1" cidr="$2"
  local subnet_ip="${cidr%/*}"
  local prefix_len="${cidr#*/}"
  local mask=$(( (0xFFFFFFFF << (32 - prefix_len)) & 0xFFFFFFFF ))

  local ip_int subnet_int
  ip_int=$(ip::to_int "${ip}")
  subnet_int=$(ip::to_int "${subnet_ip}")

  local network_addr=$(( subnet_int & mask ))
  local broadcast_addr=$(( network_addr | (~mask & 0xFFFFFFFF) ))

  if (( ip_int >= network_addr && ip_int <= broadcast_addr )); then
    return 0
  fi
  return 1
}
