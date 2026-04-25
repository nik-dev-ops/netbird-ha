package types

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	b "github.com/hashicorp/go-secure-stdlib/base62"
	"github.com/rs/xid"

	"github.com/netbirdio/netbird/base62"
	"github.com/netbirdio/netbird/management/server/util"
)

const (
	ProxyTokenPrefix            = "nbx_"
	ProxyTokenSecretLength      = 30
	ProxyTokenChecksumLength    = 6
	ProxyTokenLength            = 40
	ProxyTokenChecksumSecret    = "netbird-proxy-token-checksum-secret-v1"
)

// HashedProxyToken is a SHA-256 hash of a plain proxy token, base64-encoded.
type HashedProxyToken string

// PlainProxyToken is the raw token string displayed once at creation time.
type PlainProxyToken string

// ProxyAccessToken holds information about a proxy access token including a hashed version for verification
type ProxyAccessToken struct {
	ID          string `gorm:"primaryKey"`
	Name        string
	HashedToken HashedProxyToken `gorm:"type:varchar(255);uniqueIndex"`
	// AccountID is nil for management-wide tokens, set for account-scoped tokens
	AccountID *string `gorm:"index"`
	ExpiresAt *time.Time
	CreatedBy string
	CreatedAt time.Time
	LastUsed  *time.Time
	Revoked   bool
}

// IsExpired returns true if the token has expired
func (t *ProxyAccessToken) IsExpired() bool {
	if t.ExpiresAt == nil {
		return false
	}
	return time.Now().After(*t.ExpiresAt)
}

// IsValid returns true if the token is not revoked and not expired
func (t *ProxyAccessToken) IsValid() bool {
	return !t.Revoked && !t.IsExpired()
}

// ProxyAccessTokenGenerated holds the new token and the plain text version
type ProxyAccessTokenGenerated struct {
	PlainToken PlainProxyToken
	ProxyAccessToken
}

// CreateNewProxyAccessToken generates a new proxy access token.
// Returns the token with hashed value stored and plain token for one-time display.
func CreateNewProxyAccessToken(name string, expiresIn time.Duration, accountID *string, createdBy string) (*ProxyAccessTokenGenerated, error) {
	hashedToken, plainToken, err := generateProxyToken()
	if err != nil {
		return nil, err
	}

	currentTime := time.Now().UTC()
	var expiresAt *time.Time
	if expiresIn > 0 {
		expiresAt = util.ToPtr(currentTime.Add(expiresIn))
	}

	return &ProxyAccessTokenGenerated{
		ProxyAccessToken: ProxyAccessToken{
			ID:          xid.New().String(),
			Name:        name,
			HashedToken: hashedToken,
			AccountID:   accountID,
			ExpiresAt:   expiresAt,
			CreatedBy:   createdBy,
			CreatedAt:   currentTime,
			Revoked:     false,
		},
		PlainToken: plainToken,
	}, nil
}

func generateProxyToken() (HashedProxyToken, PlainProxyToken, error) {
	secret, err := b.Random(ProxyTokenSecretLength)
	if err != nil {
		return "", "", err
	}

	mac := hmac.New(sha256.New, []byte(ProxyTokenChecksumSecret))
	mac.Write([]byte(secret))
	sum := mac.Sum(nil)
	var cksum uint32
	for i := 0; i < 6; i++ {
		cksum = cksum*31 + uint32(sum[i])
	}
	encodedChecksum := base62.Encode(cksum)
	paddedChecksum := fmt.Sprintf("%06s", encodedChecksum)
	plainToken := PlainProxyToken(ProxyTokenPrefix + secret + paddedChecksum)
	return plainToken.Hash(), plainToken, nil
}

// Hash returns the SHA-256 hash of the plain token, base64-encoded.
func (t PlainProxyToken) Hash() HashedProxyToken {
	h := sha256.Sum256([]byte(t))
	return HashedProxyToken(base64.StdEncoding.EncodeToString(h[:]))
}

func (t PlainProxyToken) Validate() error {
	if !strings.HasPrefix(string(t), ProxyTokenPrefix) {
		return fmt.Errorf("invalid token prefix")
	}

	if len(t) != ProxyTokenLength {
		return fmt.Errorf("invalid token length")
	}

	secret := t[len(ProxyTokenPrefix) : len(t)-ProxyTokenChecksumLength]
	checksumStr := t[len(t)-ProxyTokenChecksumLength:]

	mac := hmac.New(sha256.New, []byte(ProxyTokenChecksumSecret))
	mac.Write([]byte(secret))
	sum := mac.Sum(nil)
	var cksum uint32
	for i := 0; i < 6; i++ {
		cksum = cksum*31 + uint32(sum[i])
	}
	expectedChecksum := base62.Encode(cksum)
	expectedChecksumStr := fmt.Sprintf("%06s", expectedChecksum)

	if string(checksumStr) != expectedChecksumStr {
		return fmt.Errorf("invalid token checksum")
	}

	return nil
}
