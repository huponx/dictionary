// Package identityclient keeps the preferences of a user in identity (the table identity_user_settings).
// The calls are made on behalf of the user of the request: the token of the request is exchanged for one
// for identity (see grpcclient.WithTokenExchange), identity takes the user from it.
package identityclient

import (
	"context"

	"github.com/huponx/dictionary/internal/settings"
	identityv1 "github.com/huponx/protogen/go/identity/v1"
	"google.golang.org/grpc"
)

// Client reads and writes the settings of the application "dictionary".
type Client struct {
	c identityv1.IdentityServiceClient
}

// New wraps a connection whose calls carry an exchanged token.
func New(conn grpc.ClientConnInterface) *Client {
	return &Client{c: identityv1.NewIdentityServiceClient(conn)}
}

// Get returns the stored JSON object ("{}" when there is none).
func (c *Client) Get(ctx context.Context) (string, error) {
	r, err := c.c.GetMyUserSettings(ctx, &identityv1.GetMyUserSettingsRequest{App: settings.App})
	if err != nil {
		return "", err
	}
	return r.GetSettingsJson(), nil
}

// Put replaces the stored JSON object.
func (c *Client) Put(ctx context.Context, settingsJSON string) error {
	_, err := c.c.PutMyUserSettings(ctx, &identityv1.PutMyUserSettingsRequest{App: settings.App, SettingsJson: settingsJSON})
	return err
}
