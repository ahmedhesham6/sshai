package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/auth"
	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

func TestVerifierAcceptsWorkOSAccessToken(t *testing.T) {
	fixture := newJWTFixture(t)
	verifier := fixture.verifier(t)
	token := fixture.sign(t, "user_01", "client_01", fixture.issuer, fixture.now.Add(time.Minute))

	subject, err := verifier.Verify(t.Context(), token)
	if err != nil {
		t.Fatalf("Verify(): %v", err)
	}
	if subject.WorkOSUserID != "user_01" {
		t.Fatalf("WorkOS User ID = %q, want user_01", subject.WorkOSUserID)
	}
}

func TestVerifierRejectsInvalidWorkOSClaimsAndSignature(t *testing.T) {
	fixture := newJWTFixture(t)
	verifier := fixture.verifier(t)
	other := newJWTFixture(t)
	tests := []struct {
		name  string
		token string
	}{
		{name: "missing subject", token: fixture.sign(t, "", "client_01", fixture.issuer, fixture.now.Add(time.Minute))},
		{name: "wrong client", token: fixture.sign(t, "user_01", "other-client", fixture.issuer, fixture.now.Add(time.Minute))},
		{name: "wrong issuer", token: fixture.sign(t, "user_01", "client_01", "https://attacker.example", fixture.now.Add(time.Minute))},
		{name: "expired", token: fixture.sign(t, "user_01", "client_01", fixture.issuer, fixture.now.Add(-time.Second))},
		{name: "missing expiration", token: fixture.signWithoutExpiration(t, "user_01", "client_01", fixture.issuer)},
		{name: "unknown signing key", token: other.sign(t, "user_01", "client_01", fixture.issuer, fixture.now.Add(time.Minute))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := verifier.Verify(t.Context(), test.token); err == nil {
				t.Fatal("Verify() error = nil")
			}
		})
	}
}

func TestVerifierReusesJWKSForConcurrentRequests(t *testing.T) {
	fixture := newJWTFixture(t)
	verifier := fixture.verifier(t)
	token := fixture.sign(t, "user_01", "client_01", fixture.issuer, fixture.now.Add(time.Minute))
	const requests = 32
	var group sync.WaitGroup
	errors := make(chan error, requests)
	for range requests {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := verifier.Verify(context.Background(), token)
			errors <- err
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent Verify(): %v", err)
		}
	}
	if got := fixture.jwksRequests.Load(); got != 1 {
		t.Fatalf("JWKS requests = %d, want 1 cached retrieval", got)
	}
}

type jwtFixture struct {
	now          time.Time
	issuer       string
	privateKey   jwk.Key
	server       *httptest.Server
	jwksRequests atomic.Int64
}

func newJWTFixture(t *testing.T) *jwtFixture {
	t.Helper()
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	privateKey, err := jwk.Import(private)
	if err != nil {
		t.Fatalf("import private JWK: %v", err)
	}
	if err := privateKey.Set(jwk.KeyIDKey, "key-1"); err != nil {
		t.Fatalf("set key ID: %v", err)
	}
	if err := privateKey.Set(jwk.AlgorithmKey, jwa.RS256()); err != nil {
		t.Fatalf("set key algorithm: %v", err)
	}
	publicKey, err := privateKey.PublicKey()
	if err != nil {
		t.Fatalf("derive public JWK: %v", err)
	}
	set := jwk.NewSet()
	if err := set.AddKey(publicKey); err != nil {
		t.Fatalf("add public JWK: %v", err)
	}
	encoded, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("encode JWKS: %v", err)
	}
	fixture := &jwtFixture{
		now:        time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC),
		issuer:     "https://api.workos.com/",
		privateKey: privateKey,
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		fixture.jwksRequests.Add(1)
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write(encoded)
	}))
	t.Cleanup(server.Close)
	fixture.server = server
	return fixture
}

func (fixture *jwtFixture) verifier(t *testing.T) *auth.Verifier {
	t.Helper()
	verifier, err := auth.NewVerifier(context.Background(), auth.VerifierConfig{
		JWKSURL: fixture.server.URL, Issuer: fixture.issuer, ClientID: "client_01",
		HTTPClient: fixture.server.Client(), Now: func() time.Time { return fixture.now },
	})
	if err != nil {
		t.Fatalf("NewVerifier(): %v", err)
	}
	t.Cleanup(func() { _ = verifier.Close(context.Background()) })
	return verifier
}

func (fixture *jwtFixture) sign(t *testing.T, subject, clientID, issuer string, expiresAt time.Time) string {
	t.Helper()
	token := jwt.New()
	for claim, value := range map[string]any{
		jwt.SubjectKey:    subject,
		jwt.IssuerKey:     issuer,
		jwt.ExpirationKey: expiresAt,
		"client_id":       clientID,
	} {
		if err := token.Set(claim, value); err != nil {
			t.Fatalf("set %s claim: %v", claim, err)
		}
	}
	signed, err := jwt.Sign(token, jwt.WithKey(jwa.RS256(), fixture.privateKey))
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return string(signed)
}

func (fixture *jwtFixture) signWithoutExpiration(t *testing.T, subject, clientID, issuer string) string {
	t.Helper()
	token := jwt.New()
	for claim, value := range map[string]any{
		jwt.SubjectKey: subject,
		jwt.IssuerKey:  issuer,
		"client_id":    clientID,
	} {
		if err := token.Set(claim, value); err != nil {
			t.Fatalf("set %s claim: %v", claim, err)
		}
	}
	signed, err := jwt.Sign(token, jwt.WithKey(jwa.RS256(), fixture.privateKey))
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return string(signed)
}
