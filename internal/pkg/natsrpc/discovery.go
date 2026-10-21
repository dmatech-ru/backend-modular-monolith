package natsrpc

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const DiscoveryTTL = 30 * time.Second

func RegisterDescriptors(
	ctx context.Context,
	nc *nats.Conn,
	env string,
	serviceName string,
	descriptorBytes []byte,
) error {
	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("failed to init jetstream: %w", err)
	}

	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      "service_discovery",
		Description: "NATS JetStream KV Service Discovery with Buf Descriptors",
		TTL:         DiscoveryTTL,
	})
	if err != nil {
		return fmt.Errorf("failed to get kv bucket: %w", err)
	}

	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("failed to get hostname: %w", err)
	}

	key := fmt.Sprintf("discovery.%s.%s.%s", env, serviceName, hostname)

	_, err = kv.Put(ctx, key, descriptorBytes)
	if err != nil {
		return fmt.Errorf("failed to put initial descriptor: %w", err)
	}

	pingCtx, pingCancel := context.WithCancel(ctx)

	go func() {
		defer pingCancel()

		ticker := time.NewTicker(DiscoveryTTL / 3)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				putCtx, cancel := context.WithTimeout(pingCtx, 2*time.Second)
				_, _ = kv.Put(putCtx, key, descriptorBytes)
				cancel()

			case <-pingCtx.Done():
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = kv.Delete(cleanupCtx, key)
				cancel()
				return
			}
		}
	}()

	return nil
}
