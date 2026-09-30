package auth

import (
	"os"

	"go.uber.org/fx"

	"wagering/internal/config"
)

// Module provides the Verifier and Middleware from process configuration.
// Optional environment overrides: OIDC_JWKS_URL (skip discovery, e.g. when the
// service reaches the IdP under another host than the token issuer) and
// OIDC_PROVIDER_CLAIM (default "provider_id").
var Module = fx.Module("auth",
	fx.Provide(
		OptionsFromConfig,
		fx.Annotate(New, fx.As(new(TokenVerifier)), fx.As(fx.Self())),
		NewMiddleware,
	),
)

// OptionsFromConfig derives Verifier options from the process configuration.
func OptionsFromConfig(c config.Config) Options {
	return Options{
		Issuer:        c.OIDCIssuerURL,
		Audience:      c.OIDCAudience,
		JWKSURL:       os.Getenv("OIDC_JWKS_URL"),
		ProviderClaim: os.Getenv("OIDC_PROVIDER_CLAIM"),
	}
}
