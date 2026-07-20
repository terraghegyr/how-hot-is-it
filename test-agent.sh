#!/bin/sh
# Tests for agent.sh parse_temp using fixture files. Invoked by `make test`
# (also runnable directly). Uses only POSIX sh + awk.
set -u

DIR=$(dirname "$0")
HHII_LIB=1 . "$DIR/agent.sh"

fail=0
check() {
	desc=$1
	expected=$2
	actual=$3
	if [ "$expected" = "$actual" ]; then
		echo "ok   - $desc"
	else
		echo "FAIL - $desc (expected '$expected', got '$actual')"
		fail=1
	fi
}

# Intel coretemp: max input is Package id 0 at 45.0 (fan/voltage ignored).
got=$(parse_temp < "$DIR/testdata/sensors-intel.json")
check "intel coretemp max temp" "45.0" "$got"

# AMD k10temp: max input is Tctl at 62.5.
got=$(parse_temp < "$DIR/testdata/sensors-amd.json")
check "amd k10temp max temp" "62.5" "$got"

# Empty/malformed JSON: no temperature, non-zero exit, empty output.
got=$(parse_temp < "$DIR/testdata/sensors-empty.json")
rc=$?
check "empty fixture output" "" "$got"
check "empty fixture exit code" "1" "$rc"

# Completely empty stdin also exits non-zero.
printf '' | parse_temp
rc=$?
check "empty stdin exit code" "1" "$rc"

# shellcheck is optional (not always installed in CI sandboxes).
if command -v shellcheck >/dev/null 2>&1; then
	if shellcheck "$DIR/agent.sh" "$DIR/test-agent.sh"; then
		echo "ok   - shellcheck clean"
	else
		echo "FAIL - shellcheck reported issues"
		fail=1
	fi
else
	echo "skip - shellcheck not installed"
fi

if [ "$fail" -ne 0 ]; then
	echo "AGENT TESTS FAILED"
	exit 1
fi
echo "AGENT TESTS PASSED"
