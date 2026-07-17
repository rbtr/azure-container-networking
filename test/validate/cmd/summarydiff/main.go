package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
)

var errSummaryRegression = errors.New("state validation summary regression")

type validationSummary struct {
	Checks []validationCheckEntry `json:"checks,omitempty"`
}

type validationCheckEntry struct {
	CheckName       string   `json:"checkName"`
	NodeName        string   `json:"nodeName"`
	ExpectedCount   int      `json:"expectedCount"`
	ActualCount     int      `json:"actualCount"`
	MissingIPs      []string `json:"missingIPs,omitempty"`
	UnexpectedIPs   []string `json:"unexpectedIPs,omitempty"`
	DuplicateIPs    []string `json:"duplicateIPs,omitempty"`
	ValidationPass  bool     `json:"validationPass"`
	StateBackend    string   `json:"stateBackend,omitempty"`
	Authority       string   `json:"authority,omitempty"`
	SchemaVersion   uint32   `json:"schemaVersion,omitempty"`
	Generation      uint64   `json:"generation,omitempty"`
	BootID          string   `json:"bootID,omitempty"`
	EndpointCount   int      `json:"endpointCount,omitempty"`
	AssignmentCount int      `json:"assignmentCount,omitempty"`
	OwnerCount      int      `json:"ownerCount,omitempty"`
	TombstoneCount  int      `json:"tombstoneCount,omitempty"`
}

type summaryStats struct {
	TotalChecks    int `json:"totalChecks"`
	FailedChecks   int `json:"failedChecks"`
	MissingIPs     int `json:"missingIPs"`
	UnexpectedIPs  int `json:"unexpectedIPs"`
	DuplicateIPs   int `json:"duplicateIPs"`
	ExpectedIPsSum int `json:"expectedIPsSum"`
	ActualIPsSum   int `json:"actualIPsSum"`
}

type compareOutput struct {
	Baseline  summaryStats `json:"baseline"`
	Candidate summaryStats `json:"candidate"`
}

func main() {
	baselinePath := flag.String("baseline", "", "Path to baseline validation summary JSON")
	candidatePath := flag.String("candidate", "", "Path to candidate validation summary JSON")
	flag.Parse()

	if *baselinePath == "" || *candidatePath == "" {
		fmt.Fprintln(os.Stderr, "usage: summarydiff -baseline <path> -candidate <path>")
		os.Exit(2)
	}

	baseline, err := readSummary(*baselinePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read baseline summary: %v\n", err)
		os.Exit(2)
	}
	candidate, err := readSummary(*candidatePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read candidate summary: %v\n", err)
		os.Exit(2)
	}

	output := compareOutput{Baseline: aggregate(baseline), Candidate: aggregate(candidate)}
	raw, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to encode comparison output: %v\n", err)
		os.Exit(2)
	}
	fmt.Println(string(raw))

	if err := compareSummaries(baseline, candidate); err != nil {
		fmt.Fprintf(os.Stderr, "summarydiff failed: %v\n", err)
		os.Exit(1)
	}
}

func readSummary(path string) (validationSummary, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return validationSummary{}, fmt.Errorf("reading summary %q: %w", path, err)
	}

	var summary validationSummary
	if err := json.Unmarshal(raw, &summary); err != nil {
		return validationSummary{}, fmt.Errorf("decoding summary %q: %w", path, err)
	}
	return summary, nil
}

func aggregate(summary validationSummary) summaryStats {
	stats := summaryStats{}
	for i := range summary.Checks {
		check := summary.Checks[i]
		stats.TotalChecks++
		stats.ExpectedIPsSum += check.ExpectedCount
		stats.ActualIPsSum += check.ActualCount
		stats.MissingIPs += len(check.MissingIPs)
		stats.UnexpectedIPs += len(check.UnexpectedIPs)
		stats.DuplicateIPs += len(check.DuplicateIPs)
		if !check.ValidationPass {
			stats.FailedChecks++
		}
	}
	return stats
}

func compareSummaries(baseline, candidate validationSummary) error {
	baselineStats := aggregate(baseline)
	candidateStats := aggregate(candidate)
	if candidateStats.TotalChecks != baselineStats.TotalChecks {
		return fmt.Errorf(
			"%w: total checks mismatch baseline=%d candidate=%d",
			errSummaryRegression,
			baselineStats.TotalChecks,
			candidateStats.TotalChecks,
		)
	}
	if candidateStats.FailedChecks > baselineStats.FailedChecks ||
		candidateStats.MissingIPs > baselineStats.MissingIPs ||
		candidateStats.UnexpectedIPs > baselineStats.UnexpectedIPs ||
		candidateStats.DuplicateIPs > baselineStats.DuplicateIPs {
		return fmt.Errorf("%w: candidate has worse mismatch metrics than baseline", errSummaryRegression)
	}

	baselineByCheck := make(map[string]validationCheckEntry, len(baseline.Checks))
	for i := range baseline.Checks {
		check := baseline.Checks[i]
		baselineByCheck[check.CheckName+"\x00"+check.NodeName] = check
	}
	for i := range candidate.Checks {
		check := candidate.Checks[i]
		key := check.CheckName + "\x00" + check.NodeName
		baselineCheck, ok := baselineByCheck[key]
		if !ok {
			return fmt.Errorf("%w: candidate contains unexpected check %q on node %q", errSummaryRegression, check.CheckName, check.NodeName)
		}
		if check.ExpectedCount < baselineCheck.ExpectedCount || check.ActualCount < baselineCheck.ActualCount {
			return fmt.Errorf(
				"%w: candidate check %q on node %q lost IPs: expected baseline=%d candidate=%d actual baseline=%d candidate=%d",
				errSummaryRegression,
				check.CheckName,
				check.NodeName,
				baselineCheck.ExpectedCount,
				check.ExpectedCount,
				baselineCheck.ActualCount,
				check.ActualCount,
			)
		}
		if err := comparePersistentState(baselineCheck, check); err != nil {
			return err
		}
		delete(baselineByCheck, key)
	}
	if len(baselineByCheck) != 0 {
		return fmt.Errorf("%w: candidate is missing %d baseline checks", errSummaryRegression, len(baselineByCheck))
	}
	return nil
}

func comparePersistentState(baseline, candidate validationCheckEntry) error {
	if baseline.StateBackend == "" {
		return nil
	}
	if candidate.StateBackend != baseline.StateBackend {
		return fmt.Errorf(
			"%w: check %q on node %q changed backend from %q to %q",
			errSummaryRegression,
			candidate.CheckName,
			candidate.NodeName,
			baseline.StateBackend,
			candidate.StateBackend,
		)
	}
	if candidate.Authority != baseline.Authority || candidate.SchemaVersion != baseline.SchemaVersion {
		return fmt.Errorf(
			"%w: check %q on node %q changed authority/schema from %s/%d to %s/%d",
			errSummaryRegression,
			candidate.CheckName,
			candidate.NodeName,
			baseline.Authority,
			baseline.SchemaVersion,
			candidate.Authority,
			candidate.SchemaVersion,
		)
	}
	if candidate.Generation < baseline.Generation {
		return fmt.Errorf(
			"%w: check %q on node %q generation decreased from %d to %d",
			errSummaryRegression,
			candidate.CheckName,
			candidate.NodeName,
			baseline.Generation,
			candidate.Generation,
		)
	}
	if candidate.EndpointCount < baseline.EndpointCount ||
		candidate.AssignmentCount < baseline.AssignmentCount ||
		candidate.OwnerCount < baseline.OwnerCount {
		return fmt.Errorf(
			"%w: check %q on node %q lost persistent records: endpoints %d->%d assignments %d->%d owners %d->%d",
			errSummaryRegression,
			candidate.CheckName,
			candidate.NodeName,
			baseline.EndpointCount,
			candidate.EndpointCount,
			baseline.AssignmentCount,
			candidate.AssignmentCount,
			baseline.OwnerCount,
			candidate.OwnerCount,
		)
	}
	return nil
}
