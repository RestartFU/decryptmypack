package webaccounts

import (
	"context"
	"errors"
	"strings"

	"github.com/sandertv/gophertunnel/minecraft/auth"
	"golang.org/x/oauth2"
)

// Authenticator abstracts the Microsoft device login so account behaviour can be tested offline.
type Authenticator interface {
	Begin(context.Context) (*oauth2.DeviceAuthResponse, error)
	Complete(context.Context, *oauth2.DeviceAuthResponse) (*oauth2.Token, error)
	AccountName(context.Context, *oauth2.Token) (string, error)
	Refresh(*oauth2.Token) oauth2.TokenSource
}

type liveAuthenticator struct{}

// LiveAuthenticator returns the Minecraft Android device-auth flow used by real Microsoft accounts.
func LiveAuthenticator() Authenticator {
	return liveAuthenticator{}
}

func (liveAuthenticator) Begin(ctx context.Context) (*oauth2.DeviceAuthResponse, error) {
	return auth.AndroidConfig.DeviceAuth(ctx)
}

func (liveAuthenticator) Complete(ctx context.Context, challenge *oauth2.DeviceAuthResponse) (*oauth2.Token, error) {
	return auth.AndroidConfig.DeviceAccessToken(ctx, challenge)
}

// AccountName reads the Xbox gamertag shown in the visitor's account list.
func (liveAuthenticator) AccountName(ctx context.Context, token *oauth2.Token) (string, error) {
	xboxToken, err := auth.AndroidConfig.RequestXBLToken(ctx, token, "http://xboxlive.com")
	if err != nil {
		return "", err
	}
	info := xboxToken.AuthorizationToken.UserInfo()
	for _, name := range []string{info.UniqueModernGamerTag, info.GamerTag, info.ModernGamerTag} {
		if name = strings.TrimSpace(name); name != "" {
			return name, nil
		}
	}
	return "", errors.New("Xbox profile did not return an account name")
}

func (liveAuthenticator) Refresh(token *oauth2.Token) oauth2.TokenSource {
	return auth.AndroidConfig.RefreshTokenSource(token)
}
