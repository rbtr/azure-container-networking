#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
checker="${script_dir}/check-go-coverage.awk"

run_case() {
  local name="$1"
  local minimum="$2"
  local expected_status="$3"
  local expected_output="$4"
  local report="$5"
  local output
  local status

  set +e
  output="$(
    printf '%s\n' "$report" |
      awk -v minimum="$minimum" \
        -v label="test" \
        -v threshold_name="TEST_THRESHOLD" \
        -v profile="fixture.out" \
        -f "$checker" 2>&1
  )"
  status=$?
  set -e

  if [[ "$status" -ne "$expected_status" ]]; then
    printf 'coverage checker case "%s": got status %s, want %s\n%s\n' \
      "$name" "$status" "$expected_status" "$output" >&2
    exit 1
  fi
  if [[ "$output" != *"$expected_output"* ]]; then
    printf 'coverage checker case "%s": output did not contain "%s"\n%s\n' \
      "$name" "$expected_output" "$output" >&2
    exit 1
  fi
}

valid_report=$'github.com/example/package/file.go:1:\tExample\t90.0%\ntotal:\t(statements)\t90.0%'
duplicate_report="${valid_report}"$'\ntotal:\t(statements)\t90.0%'

run_case "equal threshold" "90.0" 0 "test coverage: 90.0% (minimum 90.0%)" "$valid_report"
run_case "below threshold" "90.1" 1 "below required minimum 90.1%" "$valid_report"
run_case "missing total" "90.0" 2 "could not parse exactly one total" "package coverage only"
run_case "malformed total" "90.0" 2 "could not parse exactly one total" $'total:\t(statements)\tunknown'
run_case "duplicate total" "90.0" 2 "could not parse exactly one total" "$duplicate_report"
run_case "invalid threshold" "ninety" 2 "must be a number from 0 to 100" "$valid_report"

echo "coverage checker tests passed"
