package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/lestrrat-go/httprc/v3"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

type Subject struct {
	WorkOSUserID string
}

type VerifierConfig struct {
	JWKSURL    string
	Issuer     string
	ClientID   string
	HTTPClient *http.Client
	Now        func() time.Time
}

type Verifier struct {
	cache    *jwk.Cache
	jwksURL  string
	issuer   string
	clientID string
	clock    jwt.ClockFunc
}

func NewVerifier(ctx context.Context, config VerifierConfig) (*Verifier, error) {
	if config.JWKSURL == "" || config.Issuer == "" || config.ClientID == "" {
		return nil, errors.New("create WorkOS verifier: JWKS URL, issuer, and client ID are required")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	cache, err := jwk.NewCache(ctx, httprc.NewClient())
	if err != nil {
		return nil, fmt.Errorf("create WorkOS verifier: create JWKS cache: %w", err)
	}
	options := []jwk.RegisterOption{}
	if config.HTTPClient != nil {
		options = append(options, jwk.WithHTTPClient(config.HTTPClient))
	}
	if err := cache.Register(ctx, config.JWKSURL, options...); err != nil {
		_ = cache.Shutdown(ctx)
		return nil, fmt.Errorf("create WorkOS verifier: register JWKS: %w", err)
	}
	return &Verifier{
		cache: cache, jwksURL: config.JWKSURL, issuer: config.Issuer,
		clientID: config.ClientID, clock: jwt.ClockFunc(config.Now),
	}, nil
}

func (verifier *Verifier) Verify(ctx context.Context, encoded string) (Subject, error) {
	keys, err := verifier.cache.Lookup(ctx, verifier.jwksURL)
	if err != nil {
		return Subject{}, fmt.Errorf("verify WorkOS token: load JWKS: %w", err)
	}
	token, err := jwt.Parse([]byte(encoded),
		jwt.WithKeySet(keys),
		jwt.WithIssuer(verifier.issuer),
		jwt.WithClock(verifier.clock),
		jwt.WithRequiredClaim(jwt.ExpirationKey),
	)
	if err != nil {
		return Subject{}, fmt.Errorf("verify WorkOS token: %w", err)
	}
	subject, present := token.Subject()
	if !present || subject == "" {
		return Subject{}, errors.New("verify WorkOS token: subject is required")
	}
	var clientID string
	if err := token.Get("client_id", &clientID); err != nil || clientID != verifier.clientID {
		return Subject{}, errors.New("verify WorkOS token: client ID does not match")
	}
	return Subject{WorkOSUserID: subject}, nil
}

func (verifier *Verifier) Close(ctx context.Context) error {
	return verifier.cache.Shutdown(ctx)
}
