package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
)

type loginPrompt struct {
	userCode        string
	verificationURI string
}

type loginCredentials struct {
	accessToken  string
	refreshToken string
}

func (loginCredentials) String() string   { return "CLI credentials [redacted]" }
func (loginCredentials) GoString() string { return "main.loginCredentials{[redacted]}" }

type loginFlow interface {
	Login(context.Context, func(loginPrompt) error) (loginCredentials, error)
}

// runLogin completes CLI authorization and persists credentials only after a
// successful poll. The loginFlow seam deliberately excludes device credentials.
func runLogin(ctx context.Context, flow loginFlow, configDirectory string, output io.Writer) error {
	credentials, err := flow.Login(ctx, func(prompt loginPrompt) error {
		return displayLoginPrompt(output, prompt)
	})
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if credentials.accessToken == "" || credentials.refreshToken == "" {
		return errors.New("login: WorkOS returned incomplete credentials")
	}
	if err := persistCredentialsContext(ctx, configDirectory, credentials); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	return nil
}

var userCodePattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

func displayLoginPrompt(output io.Writer, prompt loginPrompt) error {
	verificationURI, err := url.ParseRequestURI(prompt.verificationURI)
	if err != nil || verificationURI.Scheme != "https" || verificationURI.Host == "" {
		return errors.New("WorkOS returned an invalid verification URI")
	}
	if !userCodePattern.MatchString(prompt.userCode) {
		return errors.New("WorkOS returned an invalid user code")
	}
	if _, err := fmt.Fprintf(output, "Verification URI: %s\nUser code: %s\n", verificationURI.String(), prompt.userCode); err != nil {
		return fmt.Errorf("display WorkOS authorization: %w", err)
	}
	return nil
}
