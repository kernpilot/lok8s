# shellcheck shell=bash
# keysonly.sh — the keys-only marker of the Secret plugin (lo build
# --keys-only). The plugin's keys-only run puts the annotation
# secrets.lok8s.dev/keys-only on each Secret that it emits: the Secret has
# its key names and empty values only. Such a Secret never goes into a
# domain's artifacts and is never applied.

# keysonly::has_marker <file>: look for an object with the keys-only marker
# in the YAML stream <file> (Go: build.HasKeysOnlyMarker). The marker is a
# mapping, at any depth of any document (a List item too), whose
# metadata.annotations has the key secrets.lok8s.dev/keys-only. yq parses
# the stream, so a quoted, escaped or flow-style key is the same key;
# explode(.) resolves the aliases and merge keys first. Exit status:
#   0  the stream holds the marker.
#   1  it does not.
#   2  it does not parse as YAML (yq failed): it cannot be checked.
keysonly::has_marker() {
  local out
  out=$(yq --yaml-fix-merge-anchor-to-spec '[explode(.) | .. | select(kind == "map") | .metadata | select(kind == "map") | .annotations | select(kind == "map") | has("secrets.lok8s.dev/keys-only")] | any' "${1}" 2>/dev/null) || return 2
  [[ $'\n'"${out}"$'\n' == *$'\ntrue\n'* ]] && return 0
  return 1
}
