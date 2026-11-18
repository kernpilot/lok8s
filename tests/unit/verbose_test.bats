#!/usr/bin/env bats
# verbose_test.bats — debug, error and warn (utils/verbose.sh) print the
# message as it is. A message can hold a value from a spec, an argument or
# an api answer: a backslash sequence in it ("\e[2J", "\n", "\c") stays
# text and never reaches the terminal as a control character. Only the
# colour codes of the prefix are escapes. The Go twins (internal/ui ErrorTo,
# WarnTo and DebugTo) also print the message as it is.

setup() {
  load "../test_helper"
}

# _no_esc: the output holds no ESC byte (0x1b).
_no_esc() {
  [[ "${output}" != *$'\033'* ]] || { printf 'an ESC byte in the output:\n%q\n' "${output}" >&2; return 1; }
}

@test "error: a backslash sequence in the message prints as text, with no ESC byte" {
  run error 'clear \e[2J here, \n no new line'
  assert_success
  assert_output '[error] clear \e[2J here, \n no new line'
  _no_esc
  [ "${#lines[@]}" -eq 1 ]
}

@test "error, warn, debug: no escape in any argument; \\c does not cut the line" {
  run error 'a \033[31m b' '\x1b[H \0101 \t' 'end \c cut?'
  assert_output '[error] a \033[31m b \x1b[H \0101 \t end \c cut?'
  _no_esc
  run warn 'clear \e[2J here, \n no new line'
  assert_output '[warn] clear \e[2J here, \n no new line'
  _no_esc
  DEBUG=1 run debug 'clear \e[2J here, \n no new line'
  assert_output '[debug] clear \e[2J here, \n no new line'
  _no_esc
  # Without DEBUG, debug prints nothing.
  DEBUG='' run debug 'x'
  assert_output ''
}

@test "error, warn, debug: a message that starts with a dash or holds a percent sign prints as it is" {
  run error -n -e 'x'
  assert_output '[error] -n -e x'
  run warn '%s %b %d'
  assert_output '[warn] %s %b %d'
}

# The one escape a caller needs is the colour of the prefix: on a terminal
# (and without NO_COLOR) the prefix keeps the same bytes as before, and the
# message after it stays text.
@test "error, warn, debug: on a terminal the prefix keeps its colour escapes, the message stays text" {
  verbose::color() { return 0; }
  run error 'a \e[2J b'
  assert_output $'\033[0;31m[error]\033[0m a \\e[2J b'
  run warn 'a \e[2J b'
  assert_output $'\033[0;33m[warn]\033[0m a \\e[2J b'
  DEBUG=1 run debug 'a \e[2J b'
  assert_output $'\033[0;32m[debug]\033[0m a \\e[2J b'
  # Two ESC bytes in each line: the colour and the reset of the prefix.
  local esc="${output//[!$'\033']/}"
  [ "${#esc}" -eq 2 ]
}

# The colours are fixed: a colour that the environment sets is not used,
# also when it holds an escape ("\e[2J" clears the screen, "\c" cuts the
# line with %b).
@test "error, warn, debug: a colour from the environment is not used" {
  export GREEN='\e[2J' RED='\c' YELLOW='\e[2J' NC='\c'
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  verbose::color() { return 0; }
  run error 'x'
  assert_output $'\033[0;31m[error]\033[0m x'
  run warn 'y'
  assert_output $'\033[0;33m[warn]\033[0m y'
  DEBUG=1 run debug 'z'
  assert_output $'\033[0;32m[debug]\033[0m z'
}

# A test fake can replace printf (to make a write fail): the line of the
# helper still prints, as it did with echo. The fake lives in run's
# subshell only, so the bats helpers keep the builtin.
_with_a_failing_printf() {
  printf() { return 1; }
  "$@"
}

@test "error, warn: a function named printf does not stop the line" {
  run _with_a_failing_printf error 'a \e b'
  assert_output '[error] a \e b'
  run _with_a_failing_printf warn 'c'
  assert_output '[warn] c'
  DEBUG=1 run _with_a_failing_printf debug 'd'
  assert_output '[debug] d'
}

@test "error, warn, debug: a function named printf does not stop a coloured line" {
  verbose::color() { return 0; }
  run _with_a_failing_printf error 'a \e b'
  assert_output $'\033[0;31m[error]\033[0m a \\e b'
  run _with_a_failing_printf warn 'c'
  assert_output $'\033[0;33m[warn]\033[0m c'
  DEBUG=1 run _with_a_failing_printf debug 'd'
  assert_output $'\033[0;32m[debug]\033[0m d'
}
