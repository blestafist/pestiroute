package core

import (
	"context"
	"errors"
	"slices"
)

// PolicySnapshot is the immutable authorization view associated with a trusted
// principal for one request and any candidate evaluations derived from it.
type PolicySnapshot struct {
	ID         string
	Revision   int64
	Enabled    bool
	Models     []string
	Connectors []string
	RPM        int64
	TPM        int64
}

func (p PolicySnapshot) clone() PolicySnapshot {
	p.Models = slices.Clone(p.Models)
	p.Connectors = slices.Clone(p.Connectors)
	return p
}

type PolicyStore interface {
	Snapshot(context.Context, TrustedPrincipal) (PolicySnapshot, error)
}

// AccountAuthorizer verifies that an enabled account is owned by connector.
type AccountAuthorizer interface {
	AuthorizeAccount(context.Context, string, string) error
}

// CandidateAuthorization captures a principal and policy snapshot once; its
// private fields prevent callers from changing authority between candidates.
type CandidateAuthorization struct {
	principal TrustedPrincipal
	policy    PolicySnapshot
}

func (a CandidateAuthorization) AccountingIdentity() (TrustedPrincipal, PolicySnapshot) {
	return a.principal, a.policy.clone()
}

func LoadCandidateAuthorization(ctx context.Context, store PolicyStore) (CandidateAuthorization, error) {
	principal, ok := TrustedPrincipalFromContext(ctx)
	if !ok || principal.KeyID == "" || principal.PolicyID == "" || principal.KeyRevision < 1 || principal.PolicyRevision < 1 || store == nil {
		return CandidateAuthorization{}, ErrPermissionDenied
	}
	policy, err := store.Snapshot(ctx, principal)
	if err != nil {
		if ctx.Err() != nil {
			return CandidateAuthorization{}, ctx.Err()
		}
		return CandidateAuthorization{}, ErrPermissionDenied
	}
	policy = policy.clone()
	if policy.ID != principal.PolicyID || policy.Revision != principal.PolicyRevision || !policy.Enabled {
		return CandidateAuthorization{}, ErrPermissionDenied
	}
	return CandidateAuthorization{principal: principal, policy: policy}, nil
}

type CandidateAuthorizationInput struct {
	Model     string
	Connector string
	AccountID string
	Account   AccountAuthorizer
	Scope     CapabilityScope
	Candidate EligibilityCandidate
	Required  EligibilityRequirements
}

// AuthorizeCandidate rechecks target-specific policy, account ownership and
// capability eligibility against the same request snapshot for every target.
func (a CandidateAuthorization) AuthorizeCandidate(ctx context.Context, in CandidateAuthorizationInput) error {
	if err := a.AuthorizeTarget(ctx, in.Model, in.Connector, in.AccountID, in.Account); err != nil {
		return err
	}
	return a.AuthorizeEligibility(in.Scope, in.Candidate, in.Required)
}

// AuthorizeTarget allows dispatch to reject policy/account failures before
// querying a candidate's Connector capability operation.
func (a CandidateAuthorization) AuthorizeTarget(ctx context.Context, model, connector, accountID string, account AccountAuthorizer) error {
	if a.principal.KeyID == "" || a.policy.ID != a.principal.PolicyID || a.policy.Revision != a.principal.PolicyRevision || !a.policy.Enabled ||
		!slices.Contains(a.policy.Models, model) || !slices.Contains(a.policy.Connectors, connector) || account == nil {
		return ErrPermissionDenied
	}
	if err := account.AuthorizeAccount(ctx, accountID, connector); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrPermissionDenied
	}
	return nil
}

func (a CandidateAuthorization) AuthorizeEligibility(scope CapabilityScope, candidate EligibilityCandidate, required EligibilityRequirements) error {
	if a.principal.KeyID == "" || a.policy.ID != a.principal.PolicyID || a.policy.Revision != a.principal.PolicyRevision || !a.policy.Enabled {
		return ErrPermissionDenied
	}
	return candidate.Eligible(scope, required)
}

var ErrPermissionDenied = errors.New("permission denied")
