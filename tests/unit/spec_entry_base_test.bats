#!/usr/bin/env bats
# spec_entry_base_test.bats — spec::entry_base (utils/spec.sh), key by key.
#
# spec::entry_base prints the dir name of a spec.bootstrap entry as the Go
# parser reads it. Each row below has the key, the value that bash gives and
# the value that Go gives for the same key. The Go values come from the Go
# test TestParseDirBaseOfEachKeyForm (internal/bootstrapspec/entrybase_test.go):
# it holds the same rows, parses each key with bootstrapspec.Parser.Parse (the
# parser behind the audit, the bootstrap and `lo addons`) and checks
# filepath.Base of the dir it gives. "Go rejects this key" marks a key that
# the Go parser refuses ("failed to parse addon name"). The bash parser takes
# it, and the row pins what bash gives.
#
# Each row builds the dir the way bootstrap::_parse_entry does: a key that
# starts with "/" is ${PATH_BASE}<key>, a key that starts with "./" or "../" is
# ${PATH_CLUSTERS}/<domain>/<key>, and any other key is
# ${PATH_LOK8S}/addons/<key>. PATH_BASE ends in "base", as in the Go test.

setup() {
  load "../test_helper"
  setup_tmpdir
}

teardown() {
  teardown_tmpdir
}

_GO_REJECTS="Go rejects this key"

@test "spec::entry_base: each key form gives the dir name that Go gives" {
  local PATH_BASE="${BATS_TEST_TMPDIR}/base"
  local PATH_LOK8S="${BATS_TEST_TMPDIR}/base/.lok8s"
  local PATH_CLUSTERS="${BATS_TEST_TMPDIR}/base/clusters"
  local domain="test.lok8s.dev"
  # key | bash | Go
  local -a rows=(
    ".."          ".lok8s"  "${_GO_REJECTS}"
    "addons"      "addons"  "addons"
    "a/../b"      "b"       "b"
    "./x"         "x"       "x"
    "../x"        "x"       "x"
    "/x"          "x"       "x"
    "x/"          "x"       "x"
    "x//y"        "y"       "y"
    "/"           "base"    "base"
    "///"         "base"    "base"
    ""            "addons"  "addons"
    "a b/*?[x]"   "*?[x]"   "*?[x]"
    $'a\nb'       $'a\nb'   $'a\nb'
    $'./a\nb'     $'a\nb'   $'a\nb'
    $'x\n'        $'x\n'    $'x\n'
    "-n"          "-n"      "-n"
    "-e"          "-e"      "-e"
  )
  local i key want go dir got fails=0
  for (( i = 0; i < ${#rows[@]}; i += 3 )); do
    key="${rows[i]}" want="${rows[i+1]}" go="${rows[i+2]}"
    if [[ "${key}" == /* ]]; then
      dir="${PATH_BASE}${key}"
    elif [[ "${key}" == ./* || "${key}" == ../* ]]; then
      dir="${PATH_CLUSTERS}/${domain}/${key}"
    else
      dir="${PATH_LOK8S}/addons/${key}"
    fi
    # Compare the bytes in a file: `run` and `$( … )` drop trailing newlines.
    spec::entry_base "${key}" "${dir}" > "${BATS_TEST_TMPDIR}/got"
    printf '%s\n' "${want}" > "${BATS_TEST_TMPDIR}/want"
    if ! cmp -s "${BATS_TEST_TMPDIR}/got" "${BATS_TEST_TMPDIR}/want"; then
      got="$(cat "${BATS_TEST_TMPDIR}/got" && printf x)"
      printf 'key %q: bash gives %q, want %q\n' "${key}" "${got%x}" "${want}"$'\n' >&2
      fails=$(( fails + 1 ))
    fi
    if [[ "${go}" != "${_GO_REJECTS}" && "${go}" != "${want}" ]]; then
      printf 'key %q: the row has bash %q and Go %q\n' "${key}" "${want}" "${go}" >&2
      fails=$(( fails + 1 ))
    fi
  done
  [ "${fails}" -eq 0 ]
}
