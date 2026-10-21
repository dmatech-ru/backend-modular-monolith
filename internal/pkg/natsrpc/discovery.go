package natsrpc

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const NodeHeartbeatTTL = 10 * time.Second

func RegisterDescriptors(
	ctx context.Context,
	nc *nats.Conn,
	env string,
	serviceName string,
	version string,
	descriptorBytes []byte,
) error {
	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("failed to init jetstream: %w", err)
	}

	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      "service_discovery",
		Description: "Split-routing NATS JetStream KV Service Discovery with Versioning",
		TTL:         NodeHeartbeatTTL,
	})
	if err != nil {
		return fmt.Errorf("failed to get kv bucket: %w", err)
	}

	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("failed to get hostname: %w", err)
	}

	routeKey := fmt.Sprintf("discovery.%s.routes.%s.%s", env, serviceName, version)
	nodeKey := fmt.Sprintf("discovery.%s.nodes.%s.%s.%s", env, serviceName, version, hostname)

	exists, err := hasRoute(ctx, kv, routeKey)
	if err != nil {
		return fmt.Errorf("failed to check existing routes: %w", err)
	}

	if !exists {
		_, err = kv.Put(ctx, routeKey, descriptorBytes)
		if err != nil {
			return fmt.Errorf("failed to put routes: %w", err)
		}
	}

	_, err = kv.Put(ctx, nodeKey, []byte("alive"))
	if err != nil {
		return fmt.Errorf("failed to put initial node heartbeat: %w", err)
	}

	pingCtx, pingCancel := context.WithCancel(ctx)

	go func() {
		defer pingCancel()

		ticker := time.NewTicker(NodeHeartbeatTTL / 3)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				putCtx, cancel := context.WithTimeout(pingCtx, 2*time.Second)
				_, _ = kv.Put(putCtx, nodeKey, []byte("alive"))
				cancel()

			case <-pingCtx.Done():
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()

				_ = kv.Delete(cleanupCtx, nodeKey)

				searchPattern := fmt.Sprintf("discovery.%s.nodes.%s.%s.>", env, serviceName, version)
				lister, err := kv.ListKeysFiltered(cleanupCtx, searchPattern)

				hasOtherNodes := false
				if err == nil {
					for key := range lister.Keys() {
						if key != "" {
							hasOtherNodes = true
							break
						}
					}
				}

				if !hasOtherNodes {
					_ = kv.Delete(cleanupCtx, routeKey)
				}
				return
			}
		}
	}()

	return nil
}

func hasRoute(ctx context.Context, kv jetstream.KeyValue, routeKey string) (bool, error) {
	_, err := kv.Get(ctx, routeKey)
	if err == nil {
		return true, nil
	}
	if err == jetstream.ErrKeyNotFound {
		return false, nil
	}
	return false, err
}
