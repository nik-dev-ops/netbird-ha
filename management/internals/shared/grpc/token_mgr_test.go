package grpc

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"hash"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"

	"github.com/netbirdio/netbird/management/internals/controllers/network_map/update_channel"
	"github.com/netbirdio/netbird/management/internals/server/config"
	"github.com/netbirdio/netbird/management/server/groups"
	"github.com/netbirdio/netbird/management/server/settings"
	"github.com/netbirdio/netbird/util"
)

var TurnTestHost = &config.Host{
	Proto:    config.UDP,
	URI:      "turn:turn.netbird.io:77777",
	Username: "username",
	Password: "",
}

func TestTimeBasedAuthSecretsManager_GenerateCredentials(t *testing.T) {
	ttl := util.Duration{Duration: time.Hour}
	secret := "some_secret"
	peersManager := update_channel.NewPeersUpdateManager(nil)

	rc := &config.Relay{
		Addresses:      []string{"localhost:0"},
		CredentialsTTL: ttl,
		Secret:         secret,
	}

	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)
	settingsMockManager := settings.NewMockManager(ctrl)
	groupsManager := groups.NewManagerMock()

	tested, err := NewTimeBasedAuthSecretsManager(peersManager, &config.TURNConfig{
		CredentialsTTL:       ttl,
		Secret:               secret,
		Turns:                []*config.Host{TurnTestHost},
		TimeBasedCredentials: true,
	}, rc, settingsMockManager, groupsManager)
	require.NoError(t, err)

	turnCredentials, err := tested.GenerateTurnToken()
	require.NoError(t, err)

	if turnCredentials.Payload == "" {
		t.Errorf("expected generated TURN username not to be empty, got empty")
	}
	if turnCredentials.Signature == "" {
		t.Errorf("expected generated TURN password not to be empty, got empty")
	}

	validateMAC(t, sha1.New, turnCredentials.Payload, turnCredentials.Signature, []byte(secret))

	relayCredentials, err := tested.GenerateRelayToken()
	require.NoError(t, err)

	if relayCredentials.Payload == "" {
		t.Errorf("expected generated relay payload not to be empty, got empty")
	}
	if relayCredentials.Signature == "" {
		t.Errorf("expected generated relay signature not to be empty, got empty")
	}

	hashedSecret := sha256.Sum256([]byte(secret))
	validateMAC(t, sha256.New, relayCredentials.Payload, relayCredentials.Signature, hashedSecret[:])
}

func TestTimeBasedAuthSecretsManager_SetupRefresh(t *testing.T) {
	ttl := util.Duration{Duration: 2 * time.Second}
	secret := "some_secret"
	peer := "some_peer"

	rc := &config.Relay{
		Addresses:      []string{"localhost:0"},
		CredentialsTTL: ttl,
		Secret:         secret,
	}

	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)
	settingsMockManager := settings.NewMockManager(ctrl)
	groupsManager := groups.NewManagerMock()

	tested, err := NewTimeBasedAuthSecretsManager(nil, &config.TURNConfig{
		CredentialsTTL:       ttl,
		Secret:               secret,
		Turns:                []*config.Host{TurnTestHost},
		TimeBasedCredentials: true,
	}, rc, settingsMockManager, groupsManager)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// SetupRefresh is a no-op in the stateless implementation.
	// It should not panic and should not start any background goroutines.
	tested.SetupRefresh(ctx, "someAccountID", peer)
}

func TestTimeBasedAuthSecretsManager_CancelRefresh(t *testing.T) {
	ttl := util.Duration{Duration: time.Hour}
	secret := "some_secret"
	peer := "some_peer"

	rc := &config.Relay{
		Addresses:      []string{"localhost:0"},
		CredentialsTTL: ttl,
		Secret:         secret,
	}

	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)
	settingsMockManager := settings.NewMockManager(ctrl)
	groupsManager := groups.NewManagerMock()

	tested, err := NewTimeBasedAuthSecretsManager(nil, &config.TURNConfig{
		CredentialsTTL:       ttl,
		Secret:               secret,
		Turns:                []*config.Host{TurnTestHost},
		TimeBasedCredentials: true,
	}, rc, settingsMockManager, groupsManager)
	require.NoError(t, err)

	// CancelRefresh is a no-op in the stateless implementation.
	// It should not panic even when called before SetupRefresh or multiple times.
	tested.CancelRefresh(peer)
	tested.SetupRefresh(context.Background(), "someAccountID", peer)
	tested.CancelRefresh(peer)
	tested.CancelRefresh(peer)
}

func validateMAC(t *testing.T, algo func() hash.Hash, username string, actualMAC string, key []byte) {
	t.Helper()
	mac := hmac.New(algo, key)

	_, err := mac.Write([]byte(username))
	if err != nil {
		t.Fatal(err)
	}

	expectedMAC := mac.Sum(nil)
	decodedMAC, err := base64.StdEncoding.DecodeString(actualMAC)
	if err != nil {
		t.Fatal(err)
	}
	equal := hmac.Equal(decodedMAC, expectedMAC)

	if !equal {
		t.Errorf("expected password MAC to be %s. got %s", expectedMAC, decodedMAC)
	}
}
