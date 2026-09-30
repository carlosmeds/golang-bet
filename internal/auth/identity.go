package auth

import (
	"context"
	"sort"
)

// Internal scopes understood by the service.
const (
	// ScopeProvider lets a token call provider business endpoints. It must be
	// accompanied by the provider claim.
	ScopeProvider = "wagering:provider"
	// ScopeInternal lets a token call wallet, ledger and reconciliation
	// operations. Provider tokens never carry it.
	ScopeInternal = "wagering:internal"
)

// Identity is the authenticated principal extracted from a verified token.
type Identity struct {
	Subject    string
	ClientID   string
	ProviderID string
	scopes     map[string]struct{}
}

// NewIdentity builds an Identity; it is exported for tests in other packages.
func NewIdentity(subject, providerID string, scopes ...string) Identity {
	id := Identity{Subject: subject, ProviderID: providerID, scopes: map[string]struct{}{}}
	for _, s := range scopes {
		id.scopes[s] = struct{}{}
	}
	return id
}

func (i Identity) HasScope(scope string) bool {
	_, ok := i.scopes[scope]
	return ok
}

// Scopes returns the granted scopes in sorted order.
func (i Identity) Scopes() []string {
	out := make([]string, 0, len(i.scopes))
	for s := range i.scopes {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// IsProvider reports whether the token may act as a provider.
func (i Identity) IsProvider() bool {
	return i.ProviderID != "" && i.HasScope(ScopeProvider)
}

// IsInternal reports whether the token is an internal service principal.
func (i Identity) IsInternal() bool { return i.HasScope(ScopeInternal) }

type identityKey struct{}

// WithIdentity stores the identity in ctx.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// FromContext returns the authenticated identity, if any.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// AuthorizeProvider requires a provider identity whose provider matches
// providerID (from a body, path or query). Use it for every request-supplied
// provider identifier; handlers should answer lookups of another provider's
// resources as "not found" so existence is not disclosed.
func AuthorizeProvider(ctx context.Context, providerID string) error {
	id, ok := FromContext(ctx)
	if !ok || !id.IsProvider() {
		return ErrForbidden
	}
	if providerID == "" || id.ProviderID != providerID {
		return ErrProviderMismatch
	}
	return nil
}

// ProviderScope returns the provider a request is confined to. Repository
// reads and replays must filter by it.
func ProviderScope(ctx context.Context) (string, error) {
	id, ok := FromContext(ctx)
	if !ok || !id.IsProvider() {
		return "", ErrForbidden
	}
	return id.ProviderID, nil
}
