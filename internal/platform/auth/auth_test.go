package auth_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"jungle/internal/platform/auth"
	"jungle/internal/platform/config"
)

func TestVerify(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{
			"kty": "RSA", "alg": "RS256", "kid": "test", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
		if err != nil {
			t.Errorf("encode JWKS: %v", err)
		}
	}))
	defer server.Close()

	verifier := auth.New(config.Config{OIDCIssuer: server.URL, OIDCAudience: "jungle-api", OIDCJWKSURL: server.URL + "/certs"})
	tests := []struct {
		name    string
		change  func(map[string]any)
		wantErr error
	}{
		{name: "valid provider", change: func(map[string]any) {}},
		{name: "expired", change: func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }, wantErr: auth.ErrUnauthorized},
		{name: "wrong audience", change: func(c map[string]any) { c["aud"] = "another-api" }, wantErr: auth.ErrUnauthorized},
		{name: "wrong issuer", change: func(c map[string]any) { c["iss"] = "https://another-issuer" }, wantErr: auth.ErrUnauthorized},
		{name: "not yet valid", change: func(c map[string]any) { c["nbf"] = time.Now().Add(time.Minute).Unix() }, wantErr: auth.ErrUnauthorized},
		{name: "missing provider", change: func(c map[string]any) { delete(c, "providerId") }, wantErr: auth.ErrUnauthorized},
		{name: "missing roles", change: func(c map[string]any) { delete(c, "realm_access") }, wantErr: auth.ErrUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := map[string]any{
				"iss": server.URL, "aud": "jungle-api", "sub": "service-account", "exp": time.Now().Add(time.Minute).Unix(),
				"providerId": "provider-a", "realm_access": map[string]any{"roles": []string{"provider"}},
			}

			tt.change(claims)
			raw := sign(t, key, claims)

			principal, err := verifier.Verify(t.Context(), raw)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Verify() error = %v, want %v", err, tt.wantErr)
			}

			if err == nil && (principal.ProviderID != "provider-a" || principal.Internal) {
				t.Fatalf("principal = %+v", principal)
			}
		})
	}

	for _, raw := range []string{"", "invalid", "e30.e30."} {
		if _, err := verifier.Verify(t.Context(), raw); !errors.Is(err, auth.ErrUnauthorized) {
			t.Fatalf("Verify(%q) error = %v", raw, err)
		}
	}
}

func sign(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()

	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}

	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test"}`))
	body := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(body))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}

	return body + "." + base64.RawURLEncoding.EncodeToString(signature)
}
