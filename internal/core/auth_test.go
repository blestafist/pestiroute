package core

import (
	"context"
	"testing"
)

func TestPrincipalContext(t *testing.T) {
	want := TrustedPrincipal{KeyID: "key", PolicyID: "policy", KeyRevision: 4, PolicyRevision: 9}
	got, ok := TrustedPrincipalFromContext(WithTrustedPrincipal(context.Background(), want))
	if !ok || got != want {
		t.Fatalf("TrustedPrincipalFromContext() = (%+v, %t), want (%+v, true)", got, ok, want)
	}
	if _, ok := TrustedPrincipalFromContext(context.Background()); ok {
		t.Fatal("unauthenticated context unexpectedly has a principal")
	}
}
