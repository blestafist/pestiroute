package core

import "context"

// TrustedPrincipal is immutable identity produced only by trusted key verification.
type TrustedPrincipal struct {
	KeyID, PolicyID             string
	KeyRevision, PolicyRevision int64
}

// KeyStore verifies a northbound virtual key without exposing storage types to Core.
type KeyStore interface {
	Verify(context.Context, string) (TrustedPrincipal, error)
}

type trustedPrincipalContextKey struct{}

// WithTrustedPrincipal carries verified identity through runtime execution context.
func WithTrustedPrincipal(ctx context.Context, principal TrustedPrincipal) context.Context {
	return context.WithValue(ctx, trustedPrincipalContextKey{}, principal)
}

// TrustedPrincipalFromContext returns the verified identity, if authentication ran.
func TrustedPrincipalFromContext(ctx context.Context) (TrustedPrincipal, bool) {
	principal, ok := ctx.Value(trustedPrincipalContextKey{}).(TrustedPrincipal)
	return principal, ok
}
