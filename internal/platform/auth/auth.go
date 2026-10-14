package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"jungle/internal/platform/config"
	"jungle/internal/platform/identity"
)

var ErrUnauthorized = errors.New("auth: invalid or missing credential")

type Verifier struct {
	verifier *oidc.IDTokenVerifier
	client   *http.Client
	jwksURL  string
}

// New uses explicit JWKS configuration so containers can fetch keys through an
// internal URL while still verifying the exact public issuer and audience.
func New(cfg config.Config) *Verifier {
	client := &http.Client{Timeout: 5 * time.Second}
	ctx := oidc.ClientContext(context.Background(), client)
	keys := oidc.NewRemoteKeySet(ctx, cfg.OIDCJWKSURL)

	return &Verifier{client: client, jwksURL: cfg.OIDCJWKSURL, verifier: oidc.NewVerifier(cfg.OIDCIssuer, keys, &oidc.Config{
		ClientID: cfg.OIDCAudience, SupportedSigningAlgs: []string{oidc.RS256},
	})}
}

func (v *Verifier) Verify(ctx context.Context, raw string) (identity.Principal, error) {
	if raw == "" || len(raw) > 16384 {
		return identity.Principal{}, ErrUnauthorized
	}

	token, err := v.verifier.Verify(ctx, raw)
	if err != nil {
		return identity.Principal{}, errors.Join(ErrUnauthorized, err)
	}

	var claims struct {
		ProviderID  string `json:"providerId"`
		NotBefore   int64  `json:"nbf"`
		RealmAccess struct {
			Roles []string `json:"roles"`
		} `json:"realm_access"`
	}

	if err := token.Claims(&claims); err != nil {
		return identity.Principal{}, errors.Join(ErrUnauthorized, err)
	}

	if token.Subject == "" || claims.NotBefore > time.Now().Unix() {
		return identity.Principal{}, ErrUnauthorized
	}

	principal := identity.Principal{}

	for _, role := range claims.RealmAccess.Roles {
		switch role {
		case "internal":
			principal.Internal = true
		case "provider":
			principal.ProviderID = claims.ProviderID
		}
	}

	if !principal.Internal && principal.ProviderID == "" {
		return identity.Principal{}, ErrUnauthorized
	}

	return principal, nil
}

// Ready checks the configured IdP key endpoint during process startup.
func (v *Verifier) Ready(ctx context.Context) (err error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return err
	}

	response, err := v.client.Do(request)
	if err != nil {
		return err
	}

	defer func() { err = errors.Join(err, response.Body.Close()) }()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("auth: JWKS endpoint returned status %d", response.StatusCode)
	}

	var document struct {
		Keys []json.RawMessage `json:"keys"`
	}

	if err := json.NewDecoder(io.LimitReader(response.Body, 1048576)).Decode(&document); err != nil {
		return err
	}

	if len(document.Keys) == 0 {
		return errors.New("auth: JWKS contains no keys")
	}

	return nil
}
