#!/usr/bin/env bats
# chat_test.bats: the hint of `lo chat` (libs/chat) when lochat is missing.
#
# main::chat looks for ${PATH_BIN}/lochat first, then for lochat on PATH. When
# neither has it, the hint names both places and links to the install steps
# of the guide. A user project has no lochat source, so the hint gives no
# build command. The Go twin prints the same two lines (internal/cli,
# TestChatPreflightErrors).

setup() {
  load "../test_helper"
  bats_require_minimum_version 1.5.0
  setup_tmpdir
  unset LO_CHAT_CONFIG

  import() { :; }
  export -f import
  # shellcheck source=/dev/null
  source "${_PROJECT_ROOT}/.lok8s/libs/chat"

  export PATH_BIN="${BATS_TEST_TMPDIR}/bin"
  mkdir -p "${PATH_BIN}" "${BATS_TEST_TMPDIR}/empty-path"
}

teardown() { teardown_tmpdir; }

# _chat runs main::chat with a PATH that has no lochat. Only main::chat gets
# this PATH: `run` itself needs the tools of the host PATH.
_chat() { PATH="${BATS_TEST_TMPDIR}/empty-path" main::chat "$@"; }

@test "chat: a missing lochat names the places searched and links to the install steps" {
  run --separate-stderr _chat
  assert_failure 1
  assert_output ""
  assert_equal "${stderr}" "[error] lochat binary not found in ${PATH_BIN} or on PATH.
[error] To install it, do the set-up steps at https://lok8s.io/guide/lo-chat#set-up"
}

@test "chat: a lochat in PATH_BIN passes the check, and the hint does not print" {
  printf '#!/bin/sh\nexit 0\n' > "${PATH_BIN}/lochat"
  chmod 0755 "${PATH_BIN}/lochat"
  # No chat config exists, so the next check stops main::chat before the exec.
  run --separate-stderr _chat
  assert_failure 1
  assert_equal "${stderr}" "[error] no chat config (looked for ${PATH_BASE}/lo-chat.json or ${PATH_LOK8S}/chat/defaults.json)"
}
