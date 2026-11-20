#!/usr/bin/env bats
# chat_test.bats: the lochat check of `lo chat` (libs/chat).
#
# main::chat looks for ${PATH_BIN}/lochat first, then for lochat on PATH. When
# neither has it, the hint names both places and links to the install steps
# of the guide. A user project has no lochat source, so the hint gives no
# build command. When the check passes, main::chat runs the lochat that it
# found. The Go twin has the same cases (internal/cli: TestChatPreflightErrors,
# TestChatLochatOnPath and TestChatLochatInPathBin).

setup() {
  load "../test_helper"
  bats_require_minimum_version 1.5.0
  setup_tmpdir
  unset LO_CHAT_CONFIG
  # The lib imports utils/verbose when it is sourced, and test_helper has
  # sourced it already. The no-op import is for this source call only.
  import() { :; }
  # shellcheck source=/dev/null
  source "${_PROJECT_ROOT}/.lok8s/libs/chat"
  unset -f import
  # PATH_BIN is not ${PATH_BASE}/.bin, so the tests tell the two apart.
  export PATH_BIN="${BATS_TEST_TMPDIR}/bin"
  mkdir -p "${PATH_BIN}" "${BATS_TEST_TMPDIR}/empty-path" "${BATS_TEST_TMPDIR}/on-path"
}

teardown() { teardown_tmpdir; }

# _lochat <dir>: a lochat stub in <dir> that prints the path it runs as.
_lochat() {
  printf '#!/bin/sh\nprintf "lochat ran as %%s\\n" "$0"\n' > "$1/lochat"
  chmod 0755 "$1/lochat"
}

# _ready: a chat config and an argsh.so, so that the checks after the lochat
# check pass and main::chat runs the lochat that it found.
_ready() {
  printf '{}\n' > "${PATH_BASE}/lo-chat.json"
  : > "${PATH_BIN}/argsh.so"
}

_chat() { PATH="${BATS_TEST_TMPDIR}/empty-path" main::chat "$@"; }
_chat_on_path() { PATH="${BATS_TEST_TMPDIR}/on-path" main::chat "$@"; }

@test "chat: a missing lochat names the places searched and links to the install steps" {
  run --separate-stderr _chat
  assert_failure 1
  assert_output ""
  assert_equal "${stderr}" "[error] lochat binary not found in ${PATH_BIN} or on PATH.
[error] To install it, do the set-up steps at https://lok8s.io/guide/lo-chat#set-up"
}

@test "chat: a lochat in PATH_BIN passes the check and runs, and the hint does not print" {
  _lochat "${PATH_BIN}"
  _ready
  run --separate-stderr _chat
  assert_success
  assert_output "lochat ran as ${PATH_BIN}/lochat"
  assert_equal "${stderr}" ""
}

@test "chat: with no lochat in PATH_BIN, a lochat on PATH passes the check and runs, and the hint does not print" {
  _lochat "${BATS_TEST_TMPDIR}/on-path"
  _ready
  run --separate-stderr _chat_on_path
  assert_success
  assert_output "lochat ran as ${BATS_TEST_TMPDIR}/on-path/lochat"
  assert_equal "${stderr}" ""
}

@test "chat: with a lochat in PATH_BIN and one on PATH, the one in PATH_BIN runs" {
  _lochat "${PATH_BIN}"
  _lochat "${BATS_TEST_TMPDIR}/on-path"
  _ready
  run --separate-stderr _chat_on_path
  assert_success
  assert_output "lochat ran as ${PATH_BIN}/lochat"
  assert_equal "${stderr}" ""
}
