package pcvm

import "strings"

type ActionKind string

const (
	ActionRun     ActionKind = "run"
	ActionInstall ActionKind = "install"
	ActionUpdate  ActionKind = "update"
	ActionReset   ActionKind = "reset"
)

type ActionPlan struct {
	Kind            ActionKind
	RequiresResolve bool
	Reason          string
}

// Reconcile is the single source of truth for startup-variable lifecycle.
// It has no filesystem or network side effects and is intentionally easy to
// exhaustively contract-test.
func Reconcile(current *State, target ProviderSpec, req Request, resolved *Resolved) ActionPlan {
	if current == nil {
		return ActionPlan{Kind: ActionInstall, RequiresResolve: resolved == nil, Reason: "fresh install"}
	}
	if current.Provider != target.ID {
		if resolved == nil {
			return ActionPlan{Kind: ActionUpdate, RequiresResolve: true, Reason: "provider changed"}
		}
		return transitionPlan(current, target, req, *resolved)
	}

	immutable := immutableConfigFingerprint(target, req)
	if current.ImmutableConfigHash != "" && immutable != current.ImmutableConfigHash {
		return ActionPlan{Kind: ActionReset, RequiresResolve: resolved == nil, Reason: "install-immutable configuration changed"}
	}

	selectorChanged := req.Version != current.Selector.Version || req.Build != current.Selector.Build ||
		normalizeRuntimeSelector(req.RuntimeVersion) != normalizeRuntimeSelector(current.Selector.Runtime)
	updateRequested := req.UpdateRequest != "" && hashToken(req.UpdateRequest) != current.UpdateRequestHash
	if resolved == nil && (selectorChanged || updateRequested || req.AutoUpdate) {
		reason := "selector changed"
		if updateRequested {
			reason = "UPDATE_REQUEST changed"
		} else if req.AutoUpdate {
			reason = "AUTO_UPDATE enabled"
		}
		return ActionPlan{Kind: ActionUpdate, RequiresResolve: true, Reason: reason}
	}
	if resolved != nil {
		return transitionPlan(current, target, req, *resolved)
	}
	return ActionPlan{Kind: ActionRun}
}

func transitionPlan(current *State, target ProviderSpec, req Request, resolved Resolved) ActionPlan {
	provider := &catalogProvider{spec: target}
	if drivers, err := compiledProviderDrivers(target); err == nil {
		provider.drivers = drivers
	} else {
		// Reconcile remains a pure function and is also used by focused tests
		// with intentionally minimal specs. Production catalog validation has
		// already required the complete compiled contract.
		provider.drivers.Comparator = compiledComparators[strings.ToLower(strings.TrimSpace(target.VersionDomain))]
		if provider.drivers.Comparator == nil {
			provider.drivers.Comparator = versionComparatorFunc(CompareVersions)
		}
	}
	return transitionPolicyForSpec(target).Plan(provider, current, req, resolved)
}

func normalizeRuntimeSelector(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return "auto"
	}
	return value
}

func CompareVersionsForDomain(domain, a, b string) int {
	comparator := compiledComparators[strings.ToLower(strings.TrimSpace(domain))]
	if comparator == nil && strings.EqualFold(strings.TrimSpace(domain), "none") {
		comparator = compiledComparators["opaque"]
	}
	if comparator == nil {
		comparator = versionComparatorFunc(CompareVersions)
	}
	return comparator.Compare(a, b)
}
