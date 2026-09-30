// Package auth validates external OIDC bearer tokens and authorizes requests.
//
// The service never issues credentials: tokens are verified against the
// identity provider's JWKS (signature, issuer, audience, expiry) and mapped to
// an Identity. Business endpoints are bound to the token's provider claim;
// wallet, ledger and reconciliation operations need the internal scope.
package auth
