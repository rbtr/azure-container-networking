BEGIN {
  if (label == "") {
    label = "Total"
  }
  if (threshold_name == "") {
    threshold_name = "coverage threshold"
  }
  if (profile == "") {
    profile = "coverage report"
  }

  if (minimum !~ /^[0-9]+([.][0-9]+)?$/ || minimum + 0 < 0 || minimum + 0 > 100) {
    invalid_threshold = 1
  }
}

$1 == "total:" {
  total_lines++
  candidate = $NF
  if (candidate !~ /^[0-9]+([.][0-9]+)?%$/) {
    invalid_total = 1
    next
  }

  sub(/%$/, "", candidate)
  if (candidate + 0 < 0 || candidate + 0 > 100) {
    invalid_total = 1
    next
  }
  total = candidate
}

END {
  if (invalid_threshold) {
    printf "ERROR: %s must be a number from 0 to 100; got \"%s\".\n", threshold_name, minimum > "/dev/stderr"
    exit 2
  }

  if (total_lines != 1 || invalid_total || total == "") {
    printf "ERROR: could not parse exactly one total coverage percentage from %s.\n", profile > "/dev/stderr"
    exit 2
  }

  if (total + 0 < minimum + 0) {
    printf "%s coverage: %s%% (minimum %s%%)\n", label, total, minimum > "/dev/stderr"
    printf "ERROR: %s coverage %s%% is below required minimum %s%%.\n", label, total, minimum > "/dev/stderr"
    printf "Add tests to raise %s coverage, or update %s intentionally if the baseline changes.\n", label, threshold_name > "/dev/stderr"
    exit 1
  }

  printf "%s coverage: %s%% (minimum %s%%)\n", label, total, minimum
}
