package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/redis/go-redis/v9"
)

// PubSub is the interface for cross-instance message fan-out.
// Nil implementation is used when REDIS_URL is not set (single-instance mode).
type PubSub interface {
	// Publish sends a message to the channel for the target agent.
	Publish(ctx context.Context, agentID string, msg Message) error
	// Subscribe registers a local deliver callback for the given agent.
	Subscribe(ctx context.Context, deliver func(Message), agentIDs ...string) error
	// Unsubscribe removes subscriptions for the given agent IDs.
	Unsubscribe(ctx context.Context, agentIDs ...string) error
	// Close cleans up resources.
	Close() error
}

// nopPubSub does nothing (single-instance fallback).
type nopPubSub struct{}

func (nopPubSub) Publish(_ context.Context, _ string, _ Message) error      { return nil }
func (nopPubSub) Subscribe(_ context.Context, _ func(Message), _ ...string) error { return nil }
func (nopPubSub) Unsubscribe(_ context.Context, _ ...string) error           { return nil }
func (nopPubSub) Close() error                                               { return nil }

// channelName returns the Redis pub/sub channel name for an agent.
func channelName(agentID string) string {
	return fmt.Sprintf("pincer:msg:%s", agentID)
}

// redisPubSub implements PubSub using Redis.
// Each Subscribe call creates an independent *redis.PubSub subscription
// to avoid the go-redis dynamic-subscribe / Channel() race.
type redisPubSub struct {
	client *redis.Client
}

// NewRedisPubSub creates a Redis-backed PubSub from a Redis URL.
// Returns nopPubSub if url is empty.
func NewRedisPubSub(redisURL string) PubSub {
	if redisURL == "" {
		return nopPubSub{}
	}
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Printf("hub/pubsub: invalid REDIS_URL %q: %v — falling back to nop", redisURL, err)
		return nopPubSub{}
	}
	c := redis.NewClient(opt)
	log.Printf("hub/pubsub: Redis pub/sub enabled (%s)", opt.Addr)
	return &redisPubSub{client: c}
}

func (r *redisPubSub) Publish(ctx context.Context, agentID string, msg Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("pubsub publish marshal: %w", err)
	}
	return r.client.Publish(ctx, channelName(agentID), data).Err()
}

func (r *redisPubSub) Subscribe(ctx context.Context, deliver func(Message), agentIDs ...string) error {
	if len(agentIDs) == 0 {
		return nil
	}
	channels := make([]string, len(agentIDs))
	for i, id := range agentIDs {
		channels[i] = channelName(id)
	}
	// Create a fresh subscription per agent to avoid go-redis Channel() race
	// when channels are added dynamically to a shared *redis.PubSub.
	sub := r.client.Subscribe(ctx, channels...)
	go func() {
		defer sub.Close()
		ch := sub.Channel()
		for payload := range ch {
			var msg Message
			if err := json.Unmarshal([]byte(payload.Payload), &msg); err != nil {
				log.Printf("hub/pubsub: unmarshal error: %v", err)
				continue
			}
			deliver(msg)
		}
	}()
	return nil
}

func (r *redisPubSub) Unsubscribe(_ context.Context, _ ...string) error {
	// Individual subscriptions are goroutine-scoped; they close when the
	// client disconnects (hub.Deregister → ctx cancel or WS close).
	return nil
}

func (r *redisPubSub) Close() error {
	return r.client.Close()
}
