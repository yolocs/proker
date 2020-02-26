package ingress

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	cloudevents "github.com/cloudevents/sdk-go"
	"github.com/cloudevents/sdk-go/pkg/cloudevents/client"
	cepubsub "github.com/cloudevents/sdk-go/pkg/cloudevents/transport/pubsub"
	"github.com/yolocs/proker/pkg/utils"
	"go.uber.org/zap"
	"knative.dev/eventing/pkg/broker"
)

var (
	shutdownTimeout = 1 * time.Minute
)

type Handler struct {
	Logger             *zap.SugaredLogger
	CeClient           cloudevents.Client
	Defaulter          client.EventDefaulter
	ProjectID, TopicID string

	pubsubClient cloudevents.Client
}

func (h *Handler) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	tOpts := []cepubsub.Option{
		cepubsub.WithProjectID(h.ProjectID),
		cepubsub.WithTopicID(h.TopicID),
	}
	t, err := cepubsub.New(ctx, tOpts...)
	if err != nil {
		return fmt.Errorf("failed to create cloudevents pubsub transport: %w", err)
	}
	c, err := cloudevents.NewClient(t)
	if err != nil {
		return fmt.Errorf("failed to create cloudevents client: %w", err)
	}
	h.pubsubClient = c

	errCh := make(chan error, 1)
	go func() {
		errCh <- h.CeClient.StartReceiver(ctx, h.receive)
	}()

	// Stop either if the receiver stops (sending to errCh) or if stopCh is closed.
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		break
	}

	// stopCh has been closed, we need to gracefully shutdown h.ceClient. cancel() will start its
	// shutdown, if it hasn't finished in a reasonable amount of time, just return an error.
	cancel()
	select {
	case err := <-errCh:
		return err
	case <-time.After(shutdownTimeout):
		return errors.New("timeout shutting down ceClient")
	}
}

func (h *Handler) receive(ctx context.Context, event cloudevents.Event, resp *cloudevents.EventResponse) error {
	// Setting the extension as a string as the CloudEvents sdk does not support non-string extensions.
	event.SetExtension(broker.EventArrivalTime, cloudevents.Timestamp{Time: time.Now()})
	tctx := cloudevents.HTTPTransportContextFrom(ctx)
	if tctx.Method != http.MethodPost {
		resp.Status = http.StatusMethodNotAllowed
		return nil
	}

	// tctx.URI is actually the request uri...
	if tctx.URI != "/" {
		resp.Status = http.StatusNotFound
		return nil
	}

	if h.Defaulter != nil {
		event = h.Defaulter(ctx, event)
	}

	_, _, err := h.pubsubClient.Send(utils.SendingContextFrom(ctx, tctx, ""), event)
	return err
}
