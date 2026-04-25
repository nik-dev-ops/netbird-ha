package grpc

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/netbirdio/netbird/management/internals/controllers/network_map"
	nbconfig "github.com/netbirdio/netbird/management/internals/server/config"
	"github.com/netbirdio/netbird/management/server/groups"
	"github.com/netbirdio/netbird/management/server/settings"
	auth "github.com/netbirdio/netbird/shared/relay/auth/hmac"
	authv2 "github.com/netbirdio/netbird/shared/relay/auth/hmac/v2"
)

const defaultDuration = 12 * time.Hour

// SecretsManager used to manage TURN and relay secrets
type SecretsManager interface {
	GenerateTurnToken() (*Token, error)
	GenerateRelayToken() (*Token, error)
	SetupRefresh(ctx context.Context, accountID, peerKey string)
	CancelRefresh(peerKey string)
	GetWGKey() (wgtypes.Key, error)
}

// TimeBasedAuthSecretsManager generates credentials with TTL and using pre-shared secret known to TURN server
type TimeBasedAuthSecretsManager struct {
	turnCfg         *nbconfig.TURNConfig
	relayCfg        *nbconfig.Relay
	turnHmacToken   *auth.TimedHMAC
	relayHmacToken  *authv2.Generator
	updateManager   network_map.PeersUpdateManager
	settingsManager settings.Manager
	groupsManager   groups.Manager
	wgKey           wgtypes.Key
}

type Token auth.Token

func NewTimeBasedAuthSecretsManager(updateManager network_map.PeersUpdateManager, turnCfg *nbconfig.TURNConfig, relayCfg *nbconfig.Relay, settingsManager settings.Manager, groupsManager groups.Manager) (*TimeBasedAuthSecretsManager, error) {
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return nil, err
	}

	mgr := &TimeBasedAuthSecretsManager{
		updateManager:   updateManager,
		turnCfg:         turnCfg,
		relayCfg:        relayCfg,
		settingsManager: settingsManager,
		groupsManager:   groupsManager,
		wgKey:           key,
	}

	if turnCfg != nil {
		duration := turnCfg.CredentialsTTL.Duration
		if turnCfg.CredentialsTTL.Duration <= 0 {
			log.Warnf("TURN credentials TTL is not set or invalid, using default value %s", defaultDuration)
			duration = defaultDuration
		}
		mgr.turnHmacToken = auth.NewTimedHMAC(turnCfg.Secret, duration)
	}

	if relayCfg != nil {
		duration := relayCfg.CredentialsTTL.Duration
		if relayCfg.CredentialsTTL.Duration <= 0 {
			log.Warnf("Relay credentials TTL is not set or invalid, using default value %s", defaultDuration)
			duration = defaultDuration
		}

		hashedSecret := sha256.Sum256([]byte(relayCfg.Secret))
		var err error
		if mgr.relayHmacToken, err = authv2.NewGenerator(authv2.AuthAlgoHMACSHA256, hashedSecret[:], duration); err != nil {
			log.Errorf("failed to create relay token generator: %s", err)
		}
	}

	return mgr, nil
}

// GetWGKey returns WireGuard private key used to generate peer keys
func (m *TimeBasedAuthSecretsManager) GetWGKey() (wgtypes.Key, error) {
	return m.wgKey, nil
}

// GenerateTurnToken generates new time-based secret credentials for TURN
func (m *TimeBasedAuthSecretsManager) GenerateTurnToken() (*Token, error) {
	if m.turnHmacToken == nil {
		return nil, fmt.Errorf("TURN configuration is not set")
	}
	turnToken, err := m.turnHmacToken.GenerateToken(sha1.New)
	if err != nil {
		return nil, fmt.Errorf("generate TURN token: %s", err)
	}
	return (*Token)(turnToken), nil
}

// GenerateRelayToken generates new time-based secret credentials for relay
func (m *TimeBasedAuthSecretsManager) GenerateRelayToken() (*Token, error) {
	if m.relayHmacToken == nil {
		return nil, fmt.Errorf("relay configuration is not set")
	}
	relayToken, err := m.relayHmacToken.GenerateToken()
	if err != nil {
		return nil, fmt.Errorf("generate relay token: %s", err)
	}

	return &Token{
		Payload:   string(relayToken.Payload),
		Signature: base64.StdEncoding.EncodeToString(relayToken.Signature),
	}, nil
}

// CancelRefresh is a no-op in the stateless implementation.
func (m *TimeBasedAuthSecretsManager) CancelRefresh(peerID string) {
}

// SetupRefresh is a no-op in the stateless implementation.
// Credentials are generated on-demand when peers sync or request them.
func (m *TimeBasedAuthSecretsManager) SetupRefresh(ctx context.Context, accountID, peerID string) {
}
