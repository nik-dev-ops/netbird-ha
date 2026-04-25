package update_channel

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

	"github.com/netbirdio/netbird/management/internals/controllers/network_map"
	"github.com/netbirdio/netbird/management/server/telemetry"
)

const channelBufferSize = 100

type PeersUpdateManager struct {
	// peerChannels is an update channel indexed by Peer.ID
	peerChannels map[string]chan *network_map.UpdateMessage
	// channelsMux keeps the mutex to access peerChannels
	channelsMux *sync.RWMutex
	// metrics provides method to collect application metrics
	metrics telemetry.AppMetrics
}

var _ network_map.PeersUpdateManager = (*PeersUpdateManager)(nil)

// NewPeersUpdateManager returns a new instance of PeersUpdateManager
func NewPeersUpdateManager(metrics telemetry.AppMetrics) *PeersUpdateManager {
	return &PeersUpdateManager{
		peerChannels: make(map[string]chan *network_map.UpdateMessage),
		channelsMux:  &sync.RWMutex{},
		metrics:      metrics,
	}
}

// SendUpdate sends update message to the peer's channel
func (p *PeersUpdateManager) SendUpdate(ctx context.Context, peerID string, update *network_map.UpdateMessage) {
	start := time.Now()
	var found, dropped bool

	p.channelsMux.RLock()

	defer func() {
		p.channelsMux.RUnlock()
		if p.metrics != nil {
			p.metrics.UpdateChannelMetrics().CountSendUpdateDuration(time.Since(start), found, dropped)
		}
	}()

	if channel, ok := p.peerChannels[peerID]; ok {
		found = true
		select {
		case channel <- update:
			log.WithContext(ctx).Debugf("update was sent to channel for peer %s", peerID)
		default:
			dropped = true
			log.WithContext(ctx).Warnf("channel for peer %s is %d full or closed", peerID, len(channel))
		}
	} else {
		log.WithContext(ctx).Debugf("peer %s has no channel", peerID)
	}
}

// CreateChannel creates a go channel for a given peer used to deliver updates relevant to the peer.
func (p *PeersUpdateManager) CreateChannel(ctx context.Context, peerID string) chan *network_map.UpdateMessage {
	start := time.Now()

	closed := false

	p.channelsMux.Lock()
	defer func() {
		p.channelsMux.Unlock()
		if p.metrics != nil {
			p.metrics.UpdateChannelMetrics().CountCreateChannelDuration(time.Since(start), closed)
		}
	}()

	if channel, ok := p.peerChannels[peerID]; ok {
		closed = true
		delete(p.peerChannels, peerID)
		close(channel)
	}
	// mbragin: todo shouldn't it be more? or configurable?
	channel := make(chan *network_map.UpdateMessage, channelBufferSize)
	p.peerChannels[peerID] = channel

	log.WithContext(ctx).Debugf("opened updates channel for a peer %s", peerID)

	return channel
}

func (p *PeersUpdateManager) closeChannel(ctx context.Context, peerID string) {
	if channel, ok := p.peerChannels[peerID]; ok {
		delete(p.peerChannels, peerID)
		close(channel)

		log.WithContext(ctx).Debugf("closed updates channel of a peer %s", peerID)
		return
	}

	log.WithContext(ctx).Debugf("closing updates channel: peer %s has no channel", peerID)
}

// CloseChannels closes updates channel for each given peer
func (p *PeersUpdateManager) CloseChannels(ctx context.Context, peerIDs []string) {
	start := time.Now()

	p.channelsMux.Lock()
	defer func() {
		p.channelsMux.Unlock()
		if p.metrics != nil {
			p.metrics.UpdateChannelMetrics().CountCloseChannelsDuration(time.Since(start), len(peerIDs))
		}
	}()

	for _, id := range peerIDs {
		p.closeChannel(ctx, id)
	}
}

// CloseChannel closes updates channel of a given peer
func (p *PeersUpdateManager) CloseChannel(ctx context.Context, peerID string) {
	start := time.Now()

	p.channelsMux.Lock()
	defer func() {
		p.channelsMux.Unlock()
		if p.metrics != nil {
			p.metrics.UpdateChannelMetrics().CountCloseChannelDuration(time.Since(start))
		}
	}()

	p.closeChannel(ctx, peerID)
}

// GetAllConnectedPeers returns a copy of the connected peers map
func (p *PeersUpdateManager) GetAllConnectedPeers() map[string]struct{} {
	start := time.Now()

	p.channelsMux.RLock()

	m := make(map[string]struct{})

	defer func() {
		p.channelsMux.RUnlock()
		if p.metrics != nil {
			p.metrics.UpdateChannelMetrics().CountGetAllConnectedPeersDuration(time.Since(start), len(m))
		}
	}()

	for ID := range p.peerChannels {
		m[ID] = struct{}{}
	}

	return m
}

// HasChannel returns true if peers has channel in update manager, otherwise false
func (p *PeersUpdateManager) HasChannel(peerID string) bool {
	start := time.Now()

	p.channelsMux.RLock()

	defer func() {
		p.channelsMux.RUnlock()
		if p.metrics != nil {
			p.metrics.UpdateChannelMetrics().CountHasChannelDuration(time.Since(start))
		}
	}()

	_, ok := p.peerChannels[peerID]

	return ok
}

func (p *PeersUpdateManager) CountStreams() int {
	p.channelsMux.RLock()
	defer p.channelsMux.RUnlock()
	return len(p.peerChannels)
}

// SubscribeToAccountUpdates subscribes to Redis pub/sub for account update channels.
// When a remote instance publishes an account update, the handler is invoked with the accountID.
// The subscription runs in a background goroutine and cancels when the provided context is done.
func (p *PeersUpdateManager) SubscribeToAccountUpdates(ctx context.Context, redisClient *redis.Client, accountChannelPrefix string, handler func(accountID string)) {
	if redisClient == nil {
		log.WithContext(ctx).Debug("redis client is nil, skipping account update subscription")
		return
	}

	pattern := fmt.Sprintf("%s*", accountChannelPrefix)
	pubsub := redisClient.PSubscribe(ctx, pattern)

	go func() {
		defer pubsub.Close()

		ch := pubsub.Channel()
		for {
			select {
			case <-ctx.Done():
				log.WithContext(ctx).Debug("stopping account update subscription")
				return
			case msg, ok := <-ch:
				if !ok {
					log.WithContext(ctx).Debug("account update subscription channel closed")
					return
				}

				accountID := strings.TrimPrefix(msg.Channel, accountChannelPrefix)
				if accountID == "" {
					log.WithContext(ctx).Warnf("received account update on channel %s but could not extract account ID", msg.Channel)
					continue
				}

				log.WithContext(ctx).Debugf("received remote account update for account %s", accountID)
				handler(accountID)
			}
		}
	}()
}
