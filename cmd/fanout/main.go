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
	"github.com/micro/go-micro/config"
	"github.com/micro/go-micro/config/source/file"
	"github.com/yolocs/proker/pkg/fanout"
	"github.com/yolocs/proker/pkg/purgatory"
	"go.uber.org/zap"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/signals"
)

type flags struct {
	Project         string `envconfig:"PROJECT" required:"true"`
	Topic           string `envconfig:"TOPIC" required:"true"`
	Purgatory       string `envconfig:"PURGATORY" required:"true"`
	Subscription    string `envconfig:"SUBSCRIPTION" required:"true"`
	ListenersConfig string `envconfig:"LISTENERS_CONFIG" required:"true"`
}

var logger *zap.SugaredLogger

func main() {
	// Parse the environment.
	var env flags
	if err := envconfig.Process("", &env); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	cfg, _ := logging.NewConfigFromMap(nil)
	logger, _ = logging.NewLoggerFromConfig(cfg, "proker-fanout")
	defer flush(logger)

	if err := profiler.Start(profiler.Config{
		Service:        "proker-fanout",
		ServiceVersion: "0.4",
		ProjectID:      "cshou-playground", // optional on GCP
		DebugLogging:   true,
	}); err != nil {
		logger.Errorw("Cannot start the profiler", zap.Error(err))
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(logging.WithLogger(context.Background(), logger))

	ceClient, err := newCeClinet()
	if err != nil {
		logger.Errorw("Failed to create cloudevents client, shutting down.", zap.Error(err))
		os.Exit(1)
	}

	lcfgSource := file.NewSource(file.WithPath(env.ListenersConfig))
	lcfg := config.NewConfig(config.WithSource(lcfgSource))
	logger.Infof("Got config: %v", lcfg.Map())
	defer lcfg.Close()

	fanoutHandler := &fanout.Handler{
		Logger:                 logger,
		ListenersCfg:           lcfg,
		MaxConcurrencyPerEvent: 100,
		ForwardClient:          ceClient,
		PubsubCfg: &fanout.PubsubConfig{
			ProjectID:        env.Project,
			TopicID:          env.Topic,
			SubscriptionID:   env.Subscription,
			PurgatoryTopicID: env.Purgatory,
		},
	}
	purgatoryHandler := &purgatory.Handler{
		Logger:        logger,
		ListenersCfg:  lcfg,
		ForwardClient: ceClient,
		ProjectID:     env.Project,
		TopicID:       env.Purgatory,
	}

	errChan := make(chan error, 2)
	go func() {
		if err := fanoutHandler.Start(ctx); err != nil {
			errChan <- err
		}
	}()
	go func() {
		if err := purgatoryHandler.Start(ctx); err != nil {
			errChan <- err
		}
	}()

	select {
	case err := <-errChan:
		logger.Errorw("Failed to bring up fanout handler, shutting down.", zap.Error(err))
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
