//go:generate wit-bindgen-wrpc go --out-dir bindings --package github.com/couchbase-examples/wasmcloud-provider-couchbase/bindings wit

package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	server "github.com/couchbase-examples/wasmcloud-provider-couchbase/bindings"
	"github.com/couchbase/gocb/v2"
	"github.com/wasmCloud/provider-sdk-go"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	// Handle SIGINT (CTRL+C) gracefully.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Set up OpenTelemetry.
	otelShutdown, err := setupOTelSDK(ctx)
	if err != nil {
		return err
	}
	// Handle shutdown properly so nothing leaks.
	defer func() {
		err = errors.Join(err, otelShutdown(context.Background()))
	}()

	// Initialize the provider with callbacks to track linked components
	providerHandler := Handler{
		linkedFrom:         make(map[string]map[string]string),
		clusterConnections: make(map[string]*gocb.Collection),
	}

	p, err := provider.New(
		provider.TargetLinkPut(func(link provider.InterfaceLinkDefinition) error {
			return handleNewTargetLink(&providerHandler, link)
		}),
		provider.TargetLinkDel(func(link provider.InterfaceLinkDefinition) error {
			return handleDelTargetLink(&providerHandler, link)
		}),
		provider.HealthCheck(func() string {
			return handleHealthCheck(&providerHandler)
		}),
		provider.Shutdown(func() error {
			return handleShutdown(&providerHandler)
		}),
	)
	if err != nil {
		return err
	}

	// Store the provider for use in the handlers
	providerHandler.WasmcloudProvider = p

	// Setup two channels to await RPC and control interface operations
	providerCh := make(chan error, 1)
	signalCh := make(chan os.Signal, 1)

	// Handle RPC operations
	stopFunc, err := server.Serve(p.RPCClient, &providerHandler, &providerHandler)
	if err != nil {
		p.Shutdown()
		return err
	}

	// Handle control interface operations
	go func() {
		err := p.Start()
		providerCh <- err
	}()

	// Shutdown on SIGINT
	signal.Notify(signalCh, syscall.SIGINT)

	// Run provider until either a shutdown is requested or a SIGINT is received
	select {
	case err = <-providerCh:
		stopFunc()
		return err
	case <-signalCh:
		p.Shutdown()
		stopFunc()
	}
	return nil
}

// Provider handler functions
func handleNewTargetLink(handler *Handler, link provider.InterfaceLinkDefinition) error {
	handler.Logger.Info("Handling new target link", "link", link)
	handler.linkedFrom[link.SourceID] = link.TargetConfig
	couchbaseConnectionArgs, err := validateCouchbaseConfig(link.TargetConfig, link.TargetSecrets)
	if err != nil {
		handler.Logger.Error("Invalid couchbase target config", "error", err)
		return err
	}
	handler.updateCouchbaseCluster(handler, link.SourceID, couchbaseConnectionArgs)
	return nil
}

func (h *Handler) updateCouchbaseCluster(handler *Handler, sourceId string, connectionArgs CouchbaseConnectionArgs) {
	// Connect to the cluster
	cluster, err := gocb.Connect(connectionArgs.ConnectionString, gocb.ClusterOptions{
		Username: connectionArgs.Username,
		Password: connectionArgs.Password,
	})
	if err != nil {
		handler.Logger.Error("unable to connect to couchbase cluster", "error", err)
		return
	}
	var collection *gocb.Collection
	if connectionArgs.CollectionName != "" && connectionArgs.ScopeName != "" {
		collection = cluster.Bucket(connectionArgs.BucketName).Scope(connectionArgs.ScopeName).Collection(connectionArgs.CollectionName)
	} else {
		collection = cluster.Bucket(connectionArgs.BucketName).DefaultCollection()
	}

	bucket := cluster.Bucket(connectionArgs.BucketName)
	if err = bucket.WaitUntilReady(5*time.Second, nil); err != nil {
		handler.Logger.Error("unable to connect to couchbase bucket", "error", err)
	}

	// Store the connection
	handler.clusterConnections[sourceId] = collection
}

func handleDelTargetLink(handler *Handler, link provider.InterfaceLinkDefinition) error {
	handler.Logger.Info("Handling del target link", "link", link)
	delete(handler.linkedFrom, link.Target)
	return nil
}

func handleHealthCheck(handler *Handler) string {
	handler.Logger.Debug("Handling health check")
	return "provider healthy"
}

func handleShutdown(handler *Handler) error {
	handler.Logger.Info("Handling shutdown")
	// clear(handler.linkedFrom)
	return nil
}
