package main

import (
	"context"
	"fmt"

	"github.com/ahmedhesham6/sshai/libs/auth"
)

type workOSLoginFlow struct {
	flow *auth.DeviceFlow
}

func newWorkOSLoginFlow(clientID string) (loginFlow, error) {
	flow, err := auth.NewDeviceFlow(clientID)
	if err != nil {
		return nil, err
	}
	return workOSLoginFlow{flow: flow}, nil
}

func (flow workOSLoginFlow) Login(ctx context.Context, display func(loginPrompt) error) (loginCredentials, error) {
	authorization, err := flow.flow.Authorize(ctx)
	if err != nil {
		return loginCredentials{}, err
	}
	if err := display(loginPrompt{userCode: authorization.UserCode(), verificationURI: authorization.VerificationURI()}); err != nil {
		return loginCredentials{}, err
	}
	tokens, err := flow.flow.Poll(ctx, authorization)
	if err != nil {
		return loginCredentials{}, fmt.Errorf("wait for WorkOS authorization: %w", err)
	}
	return loginCredentials{accessToken: tokens.AccessToken(), refreshToken: tokens.RefreshToken()}, nil
}
