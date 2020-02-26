package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"cloud.google.com/go/profiler"
	cloudevents "github.com/cloudevents/sdk-go"
	"github.com/kelseyhightower/envconfig"
	"github.com/yolocs/proker/pkg/ingress"
	"go.uber.org/zap"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/signals"
)

type flags struct {
	Project string `envconfig:"PROJECT" required:"true"`
	Topic   string `envconfig:"TOPIC" required:"true"`
}

var logger *zap.SugaredLogger

func main() {
	// Parse the environment.
	var env flags
	if err := envconfig.Process("", &env); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if err := profiler.Start(profiler.Config{
		Service:        "proker-ingress",
		ServiceVersion: "0.4",
		ProjectID:      "cshou-playground", // optional on GCP
		DebugLogging:   true,
	}); err != nil {
		logger.Errorw("Cannot start the profiler", zap.Error(err))
		os.Exit(1)
	}

	cfg, _ := logging.NewConfigFromMap(nil)
	logger, _ = logging.NewLoggerFromConfig(cfg, "proker-ingress")
	defer flush(logger)

	ctx, cancel := context.WithCancel(logging.WithLogger(context.Background(), logger))

	ceClient, err := newCeClinet()
	if err != nil {
		logger.Errorw("Failed to create cloudevents client, shutting down.", zap.Error(err))
		os.Exit(1)
	}

	handler := &ingress.Handler{
		Logger:    logger,
		CeClient:  ceClient,
		ProjectID: env.Project,
		TopicID:   env.Topic,
	}

	errChan := make(chan error, 1)
	go func() {
		if err := handler.Start(ctx); err != nil {
			errChan <- err
		}
	}()

	select {
	case err := <-errChan:
		logger.Errorw("Failed to bring up ingress handler, shutting down.", zap.Error(err))
		flush(logger)
		os.Exit(1)
	case <-signals.SetupSignalHandler():
		logger.Info("Received TERM signal, attempting to gracefully shutdown servers.")
		flush(logger)
		cancel()
		select {
		case <-errChan:
			logger.Info("Gracefully shutdown")
		case <-time.After(time.Minute):
			logger.Error("Failed to shutdown within grace period")
		}
	}
}

func newCeClinet() (cloudevents.Client, error) {
	base := http.DefaultTransport
	baseTransport := base.(*http.Transport)
	baseTransport.MaxIdleConns = 1000
	baseTransport.MaxIdleConnsPerHost = 100

	t, err := cloudevents.NewHTTPTransport(cloudevents.WithBinaryEncoding())
	if err != nil {
		return nil, fmt.Errorf("failed to create ce http transport: %w", err)
	}
	t.Client = &http.Client{
		Transport: base,
	}

	return cloudevents.NewClient(t, cloudevents.WithUUIDs(), cloudevents.WithTimeNow())
}

func flush(logger *zap.SugaredLogger) {
	logger.Sync()
	os.Stdout.Sync()
	os.Stderr.Sync()
}
