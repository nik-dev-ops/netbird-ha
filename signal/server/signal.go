package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	gproto "google.golang.org/protobuf/proto"

	"github.com/redis/go-redis/v9"
	"github.com/netbirdio/signal-dispatcher/dispatcher"

	"github.com/netbirdio/netbird/shared/distributed"
	"github.com/netbirdio/netbird/shared/signal/proto"
	"github.com/netbirdio/netbird/signal/metrics"
	"github.com/netbirdio/netbird/signal/peer"
)

const (
	labelType              = "type"
	labelTypeError         = "error"
	labelTypeNotConnected  = "not_connected"
	labelTypeNotRegistered = "not_registered"
	labelTypeStream        = "stream"
	labelTypeMessage       = "message"
	labelTypeTimeout       = "timeout"
	labelTypeDisconnected  = "disconnected"

	labelError                   = "error"
	labelErrorMissingId          = "missing_id"
	labelErrorMissingMeta        = "missing_meta"
	labelErrorFailedHeader       = "failed_header"
	labelErrorFailedRegistration = "failed_registration"

	labelRegistrationStatus   = "status"
	labelRegistrationFound    = "found"
	labelRegistrationNotFound = "not_found"

	sendTimeout = 10 * time.Second
)

var (
	ErrPeerRegisteredAgain = errors.New("peer registered again")
)

// Server an instance of a Signal server
type Server struct {
	registry *peer.Registry
	proto.UnimplementedSignalExchangeServer
	dispatcher *dispatcher.Dispatcher
	metrics    *metrics.AppMetrics

	successHeader metadata.MD

	sendTimeout time.Duration

	// HA fields (nil when HA disabled)
	haConfig     *SignalHAConfig
	redisClient  *distributed.Client
	instanceID   string
	haCtx        context.Context
	haCancel     context.CancelFunc
	haWg         sync.WaitGroup
}

// NewServer creates a new Signal server
func NewServer(ctx context.Context, meter metric.Meter, haConfig *SignalHAConfig, metricsPrefix ...string) (*Server, error) {
	appMetrics, err := metrics.NewAppMetrics(meter, metricsPrefix...)
	if err != nil {
		return nil, fmt.Errorf("creating app metrics: %v", err)
	}

	d, err := dispatcher.NewDispatcher(ctx, meter)
	if err != nil {
		return nil, fmt.Errorf("creating dispatcher: %v", err)
	}

	sTimeout := sendTimeout
	to := os.Getenv("NB_SIGNAL_SEND_TIMEOUT")
	if parsed, err := time.ParseDuration(to); err == nil && parsed > 0 {
		log.Trace("using custom send timeout ", parsed)
		sTimeout = parsed
	}

	s := &Server{
		dispatcher:    d,
		registry:      peer.NewRegistry(appMetrics),
		metrics:       appMetrics,
		successHeader: metadata.Pairs(proto.HeaderRegistered, "1"),
		sendTimeout:   sTimeout,
		haConfig:      haConfig,
	}

	// Initialize HA if enabled
	if haConfig != nil && haConfig.Enabled {
		if err := s.initHA(ctx); err != nil {
			return nil, fmt.Errorf("initializing HA: %w", err)
		}
	}

	return s, nil
}

func (s *Server) initHA(ctx context.Context) error {
	if err := s.haConfig.Validate(); err != nil {
		return err
	}

	client, err := distributed.NewClient(s.haConfig.HAConfig)
	if err != nil {
		return fmt.Errorf("connecting to redis: %w", err)
	}

	s.redisClient = client
	s.instanceID = s.haConfig.InstanceID
	s.haCtx, s.haCancel = context.WithCancel(ctx)

	// Subscribe to instance channel
	channel := distributed.SanitizeRedisKey(s.haConfig.ChannelPrefix + s.instanceID)
	pubsub := client.Subscribe(s.haCtx, channel)

	// Start message listener
	s.haWg.Add(1)
	go s.haMessageListener(pubsub)

	log.Infof("Signal HA initialized: instance=%s, redis=%s", s.instanceID, s.haConfig.RedisAddress)
	return nil
}

// Send forwards a message to the signal peer
func (s *Server) Send(ctx context.Context, msg *proto.EncryptedMessage) (*proto.EncryptedMessage, error) {
	log.Tracef("received a new message to send from peer [%s] to peer [%s]", msg.Key, msg.RemoteKey)

	// HA path: always check distributed registry when HA is enabled
	// This avoids stale local entries causing message loss or misrouting
	if s.redisClient != nil {
		instanceID, err := s.lookupPeerInstance(ctx, msg.RemoteKey)
		if err == nil && instanceID != "" {
			if instanceID == s.instanceID {
				// Peer is on this instance - check local registry
				if _, found := s.registry.Get(msg.RemoteKey); found {
					s.forwardMessageToPeer(ctx, msg)
				}
				return &proto.EncryptedMessage{}, nil
			}

			// Forward to remote instance
			envelope := signalEnvelope{
				FromInstance: s.instanceID,
				ToPeer:       msg.RemoteKey,
				Message:      msg,
			}
			if err := s.signEnvelope(&envelope); err != nil {
				log.Warnf("failed to sign envelope for peer %s: %v", msg.RemoteKey, err)
				return &proto.EncryptedMessage{}, err
			}
			payload, err := json.Marshal(envelope)
			if err != nil {
				log.Warnf("failed to marshal envelope for peer %s: %v", msg.RemoteKey, err)
				return &proto.EncryptedMessage{}, err
			}

			channel := distributed.SanitizeRedisKey(s.haConfig.ChannelPrefix + instanceID)
			if err := s.redisClient.Publish(ctx, channel, payload).Err(); err != nil {
				log.Warnf("failed to publish message to instance %s: %v", instanceID, err)
				return &proto.EncryptedMessage{}, err
			}
			log.Tracef("forwarded message to peer %s on instance %s", msg.RemoteKey, instanceID)
			return &proto.EncryptedMessage{}, nil
		}
		// Peer not in Redis - fall through to dispatcher
	} else {
		// Non-HA mode: use local registry fast path
		if _, found := s.registry.Get(msg.RemoteKey); found {
			s.forwardMessageToPeer(ctx, msg)
			return &proto.EncryptedMessage{}, nil
		}
	}

	// Fallback: try dispatcher (legacy behavior)
	return s.dispatcher.SendMessage(ctx, msg)
}

func (s *Server) lookupPeerInstance(ctx context.Context, peerID string) (string, error) {
	backoff := 100 * time.Millisecond
	maxBackoff := 400 * time.Millisecond
	maxTotalTime := 3 * time.Second
	deadline := time.Now().Add(maxTotalTime)

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "", context.DeadlineExceeded
		}

		lookupCtx, cancel := context.WithTimeout(ctx, remaining)
		instanceID, err := s.redisClient.HGet(lookupCtx, distributed.SanitizeRedisKey(s.haConfig.RegistryKey), distributed.SanitizeRedisKey(peerID)).Result()
		cancel()

		if err == nil && instanceID != "" {
			return instanceID, nil
		}

		if remaining <= backoff {
			return "", context.DeadlineExceeded
		}

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return "", ctx.Err()
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// ConnectStream connects to the exchange stream
func (s *Server) ConnectStream(stream proto.SignalExchange_ConnectStreamServer) error {
	ctx, cancel := context.WithCancel(context.Background())
	p, err := s.RegisterPeer(stream, cancel)
	if err != nil {
		return err
	}

	defer s.DeregisterPeer(p)

	// needed to confirm that the peer has been registered so that the client can proceed
	err = stream.SendHeader(s.successHeader)
	if err != nil {
		s.metrics.RegistrationFailures.Add(stream.Context(), 1, metric.WithAttributes(attribute.String(labelError, labelErrorFailedHeader)))
		return err
	}

	log.Debugf("peer connected [%s] [streamID %d] ", p.Id, p.StreamID)

	select {
	case <-stream.Context().Done():
		log.Debugf("peer stream closing [%s] [streamID %d] ", p.Id, p.StreamID)
		return nil
	case <-ctx.Done():
		return ErrPeerRegisteredAgain
	}
}

func (s *Server) RegisterPeer(stream proto.SignalExchange_ConnectStreamServer, cancel context.CancelFunc) (*peer.Peer, error) {
	log.Debugf("registering new peer")
	id := metadata.ValueFromIncomingContext(stream.Context(), proto.HeaderId)
	if id == nil {
		s.metrics.RegistrationFailures.Add(stream.Context(), 1, metric.WithAttributes(attribute.String(labelError, labelErrorMissingId)))
		return nil, status.Errorf(codes.FailedPrecondition, "missing connection header: %s", proto.HeaderId)
	}

	p := peer.NewPeer(id[0], stream, cancel)
	if err := s.registry.Register(p); err != nil {
		return nil, err
	}

	// Register in distributed registry
	s.registerPeerInRedis(p.Id)

	err := s.dispatcher.ListenForMessages(stream.Context(), p.Id, s.forwardMessageToPeer)
	if err != nil {
		s.metrics.RegistrationFailures.Add(stream.Context(), 1, metric.WithAttributes(attribute.String(labelError, labelErrorFailedRegistration)))
		log.Errorf("error while registering message listener for peer [%s] %v", p.Id, err)
		return nil, status.Errorf(codes.Internal, "error while registering message listener")
	}
	return p, nil
}

func (s *Server) DeregisterPeer(p *peer.Peer) {
	log.Debugf("peer disconnected [%s] [streamID %d] ", p.Id, p.StreamID)
	s.metrics.PeerConnectionDuration.Record(p.Stream.Context(), int64(time.Since(p.RegisteredAt).Seconds()))
	s.registry.Deregister(p)

	// Deregister from distributed registry
	s.deregisterPeerFromRedis(p.Id)
}

func (s *Server) registerPeerInRedis(peerID string) {
	if s.redisClient == nil {
		return
	}

	ctx, cancel := context.WithTimeout(s.haCtx, 5*time.Second)
	defer cancel()

	sanitizedPeerID := distributed.SanitizeRedisKey(peerID)
	sanitizedKey := distributed.SanitizeRedisKey(s.haConfig.RegistryKey)
	if err := s.redisClient.HSet(ctx, sanitizedKey, sanitizedPeerID, s.instanceID).Err(); err != nil {
		log.Warnf("failed to register peer %s in redis: %v", peerID, err)
		return
	}
	if err := s.redisClient.Expire(ctx, sanitizedKey, s.haConfig.PeerTTL).Err(); err != nil {
		log.Warnf("failed to set TTL for peer %s: %v", peerID, err)
		s.redisClient.HDel(ctx, sanitizedKey, sanitizedPeerID)
		return
	}

	s.haWg.Add(1)
	go s.peerHeartbeat(peerID)
}

func (s *Server) peerHeartbeat(peerID string) {
	defer s.haWg.Done()

	ticker := time.NewTicker(s.haConfig.HeartbeatInterval)
	defer ticker.Stop()

	sanitizedPeerID := distributed.SanitizeRedisKey(peerID)
	sanitizedKey := distributed.SanitizeRedisKey(s.haConfig.RegistryKey)
	for {
		select {
		case <-ticker.C:
			if _, found := s.registry.Get(peerID); !found {
				return
			}
			ctx, cancel := context.WithTimeout(s.haCtx, 5*time.Second)
			err := s.redisClient.HSet(ctx, sanitizedKey, sanitizedPeerID, s.instanceID).Err()
			if err == nil {
				s.redisClient.Expire(ctx, sanitizedKey, s.haConfig.PeerTTL)
			}
			cancel()
		case <-s.haCtx.Done():
			return
		}
	}
}

func (s *Server) deregisterPeerFromRedis(peerID string) {
	if s.redisClient == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.redisClient.HDel(ctx, distributed.SanitizeRedisKey(s.haConfig.RegistryKey), distributed.SanitizeRedisKey(peerID)).Err(); err != nil {
		log.Warnf("failed to deregister peer %s from redis: %v", peerID, err)
	}
}

type signalEnvelope struct {
	FromInstance string                  `json:"from_instance"`
	ToPeer       string                  `json:"to_peer"`
	Message      *proto.EncryptedMessage `json:"message"`
	Signature    string                  `json:"signature,omitempty"`
}

func (s *Server) signEnvelope(envelope *signalEnvelope) error {
	if s.haConfig.Secret == "" {
		return nil
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	h := hmac.New(sha256.New, []byte(s.haConfig.Secret))
	h.Write(data)
	envelope.Signature = hex.EncodeToString(h.Sum(nil))
	return nil
}

func (s *Server) verifyEnvelope(envelope *signalEnvelope) bool {
	if s.haConfig.Secret == "" || envelope.Signature == "" {
		return true
	}
	sig := envelope.Signature
	envelope.Signature = ""
	data, err := json.Marshal(envelope)
	if err != nil {
		return false
	}
	h := hmac.New(sha256.New, []byte(s.haConfig.Secret))
	h.Write(data)
	expected := hex.EncodeToString(h.Sum(nil))
	envelope.Signature = sig
	return hmac.Equal([]byte(sig), []byte(expected))
}

func (s *Server) haMessageListener(pubsub *redis.PubSub) {
	defer s.haWg.Done()

	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-s.haCtx.Done():
			return
		default:
		}

		ch := pubsub.Channel()
		for msg := range ch {
			if msg == nil {
				continue
			}

			var envelope signalEnvelope
			if err := json.Unmarshal([]byte(msg.Payload), &envelope); err != nil {
				log.Warnf("failed to unmarshal HA message: %v", err)
				continue
			}

			if !s.verifyEnvelope(&envelope) {
				log.Warnf("invalid signature on HA message, dropping")
				continue
			}

			s.forwardMessageToPeer(s.haCtx, envelope.Message)
		}

		// PubSub channel closed, connection lost — attempt reconnect with backoff
		if s.haCtx.Err() != nil {
			return
		}

		log.Warnf("HA pubsub connection lost, reconnecting in %v...", backoff)
		select {
		case <-time.After(backoff):
		case <-s.haCtx.Done():
			return
		}

		// Exponential backoff, capped at max
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}

		channel := distributed.SanitizeRedisKey(s.haConfig.ChannelPrefix + s.instanceID)
		newPubsub := s.redisClient.Subscribe(s.haCtx, channel)
		if newPubsub == nil {
			continue
		}
		newCh := newPubsub.Channel()
		if newCh == nil {
			continue
		}
		pubsub = newPubsub
	}
}

// Shutdown gracefully shuts down the HA components.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.haCancel != nil {
		s.haCancel()
	}

	// Wait for goroutines with timeout
	done := make(chan struct{})
	go func() {
		s.haWg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		log.Warn("HA shutdown timed out")
	}

	if s.redisClient != nil {
		_ = s.redisClient.Close()
	}

	return nil
}

func (s *Server) forwardMessageToPeer(ctx context.Context, msg *proto.EncryptedMessage) {
	log.Tracef("forwarding a new message from peer [%s] to peer [%s]", msg.Key, msg.RemoteKey)
	getRegistrationStart := time.Now()

	// lookup the target peer where the message is going to
	dstPeer, found := s.registry.Get(msg.RemoteKey)

	if !found {
		s.metrics.GetRegistrationDelay.Record(ctx, float64(time.Since(getRegistrationStart).Nanoseconds())/1e6, metric.WithAttributes(attribute.String(labelType, labelTypeStream), attribute.String(labelRegistrationStatus, labelRegistrationNotFound)))
		s.metrics.MessageForwardFailures.Add(ctx, 1, metric.WithAttributes(attribute.String(labelType, labelTypeNotConnected)))
		log.Tracef("message from peer [%s] can't be forwarded to peer [%s] because destination peer is not connected", msg.Key, msg.RemoteKey)
		// todo respond to the sender?
		return
	}

	s.metrics.GetRegistrationDelay.Record(ctx, float64(time.Since(getRegistrationStart).Nanoseconds())/1e6, metric.WithAttributes(attribute.String(labelType, labelTypeStream), attribute.String(labelRegistrationStatus, labelRegistrationFound)))
	start := time.Now()

	sendResultChan := make(chan error, 1)
	go func() {
		select {
		case sendResultChan <- dstPeer.Stream.Send(msg):
			return
		case <-dstPeer.Stream.Context().Done():
			return
		}
	}()

	select {
	case err := <-sendResultChan:
		if err != nil {
			log.Tracef("error while forwarding message from peer [%s] to peer [%s]: %v", msg.Key, msg.RemoteKey, err)
			s.metrics.MessageForwardFailures.Add(ctx, 1, metric.WithAttributes(attribute.String(labelType, labelTypeError)))
			return
		}
		s.metrics.MessageForwardLatency.Record(ctx, float64(time.Since(start).Nanoseconds())/1e6, metric.WithAttributes(attribute.String(labelType, labelTypeStream)))
		s.metrics.MessagesForwarded.Add(ctx, 1)
		s.metrics.MessageSize.Record(ctx, int64(gproto.Size(msg)), metric.WithAttributes(attribute.String(labelType, labelTypeMessage)))

	case <-dstPeer.Stream.Context().Done():
		log.Tracef("failed to forward message from peer [%s] to peer [%s]: destination peer disconnected", msg.Key, msg.RemoteKey)
		s.metrics.MessageForwardFailures.Add(ctx, 1, metric.WithAttributes(attribute.String(labelType, labelTypeDisconnected)))

	case <-time.After(s.sendTimeout):
		dstPeer.Cancel() // cancel the peer context to trigger deregistration
		log.Tracef("failed to forward message from peer [%s] to peer [%s]: send timeout", msg.Key, msg.RemoteKey)
		s.metrics.MessageForwardFailures.Add(ctx, 1, metric.WithAttributes(attribute.String(labelType, labelTypeTimeout)))
	}
}
