package filter

import "github.com/Azure/azure-container-networking/cns"

type IPConfigStatePredicate func(ipconfig cns.IPConfigurationStatus) bool

var (
	StateAllocated          = filters[cns.Allocated]
	StateAvailable          = filters[cns.Available]
	StatePendingProgramming = filters[cns.PendingProgramming]
	StatePendingRelease     = filters[cns.PendingRelease]
)

var filters = map[cns.IPConfigState]IPConfigStatePredicate{
	cns.Allocated:          ipConfigStatePredicate(cns.Allocated),
	cns.Available:          ipConfigStatePredicate(cns.Available),
	cns.PendingProgramming: ipConfigStatePredicate(cns.PendingProgramming),
	cns.PendingRelease:     ipConfigStatePredicate(cns.PendingRelease),
}

// ipConfigStatePredicate returns a predicate function that compares an IPConfigurationStatus.State to
// the passed State string and returns true when equal.
func ipConfigStatePredicate(test cns.IPConfigState) IPConfigStatePredicate {
	return func(ipconfig cns.IPConfigurationStatus) bool {
		return ipconfig.State == test
	}
}

func matchesAnyIPConfigState(in cns.IPConfigurationStatus, predicates ...IPConfigStatePredicate) bool {
	for _, p := range predicates {
		if p(in) {
			return true
		}
	}
	return false
}

// MatchAnyIPConfigState filters the passed IPConfigurationStatus map
// according to the passed predicates and returns the matching values.
func MatchAnyIPConfigState(in map[string]cns.IPConfigurationStatus, predicates ...IPConfigStatePredicate) []cns.IPConfigurationStatus {
	out := []cns.IPConfigurationStatus{}

	if len(predicates) == 0 || len(in) == 0 {
		return out
	}

	for _, v := range in {
		if matchesAnyIPConfigState(v, predicates...) {
			out = append(out, v)
		}
	}
	return out
}

// PredicatesForStates returns a slice of IPConfigStatePredicates matches
// that map to the input IPConfigStates.
func PredicatesForStates(states ...cns.IPConfigState) []IPConfigStatePredicate {
	var predicates []IPConfigStatePredicate
	for _, state := range states {
		if f, ok := filters[state]; ok {
			predicates = append(predicates, f)
		}
	}
	return predicates
}
