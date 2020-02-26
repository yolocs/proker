package purgatory

import (
	"context"
	"fmt"
	"sync"
	"time"

	"cloud.google.com/go/pubsub"
	cloudevents "github.com/cloudevents/sdk-go"
	cepubsub "github.com/cloudevents/sdk-go/pkg/cloudevents/transport/pubsub"
	"github.com/micro/go-micro/config"
	"github.com/yolocs/proker/pkg/utils"
	"go.uber.org/zap"
)

type Puller struct {
	PubsubClient  cloudevents.Client
	ForwardClient cloudevents.Client
	CancelFunc    func()
	Listener      utils.Listener
	ErrCh         chan error
}

type Handler struct {
	Logger             *zap.SugaredLogger
	ListenersCfg       config.Config
	ProjectID, TopicID string
	ForwardClient      cloudevents.Client

	pullersMap *sync.Map
}

func (h *Handler) Start(ctx context.Context) error {
	h.pullersMap = &sync.Map{}

	var listeners map[string]utils.Listener
	if err := h.ListenersCfg.Scan(&listeners); err != nil {
		return fmt.Errorf("failed to decode listener config: %w", err)
	}
	if err := h.reconcilePullers(ctx, listeners); err != nil {
		return err
	}

	w, err := h.ListenersCfg.Watch()
	if err != nil {
		return err
	}

	go func() {
		for {
			h.Logger.Info("Waiting for config updates...")
			v, err := w.Next()
			if err != nil {
				h.Logger.Error("failed to get next value from the config", zap.Error(err))
			}
			h.Logger.Info("Listeners config got refreshed...")
			var listeners map[string]utils.Listener
			if err := v.Scan(&listeners); err != nil {
				h.Logger.Error("failed to scan next value from the config", zap.Error(err))
			}
			if err := h.reconcilePullers(ctx, listeners); err != nil {
				h.Logger.Error("failed to reconcile pullers", zap.Error(err))
			}
			h.Logger.Info("Pullers reconciled...")
		}
	}()
	return nil
}

func (h *Handler) reconcilePullers(ctx context.Context, listeners map[string]utils.Listener) error {
	var errs []error
	for id, listener := range listeners {
		if _, ok := h.pullersMap.Load(id); !ok {
			ctx, cancel := context.WithCancel(ctx)
			tOpts := []cepubsub.Option{
				cepubsub.WithProjectID(h.ProjectID),
				cepubsub.WithTopicID(h.TopicID),
				cepubsub.WithSubscriptionAndTopicID(listener.PurgatorySubscription, h.TopicID),
			}
			t, err := cepubsub.New(ctx, tOpts...)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to create cloudevents pubsub transport: %w", err))
			}
			t.ReceiveSettings = &pubsub.ReceiveSettings{
				NumGoroutines: 1,
			}

			c, err := cloudevents.NewClient(t)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to create cloudevents client: %w", err))
			}
			p := &Puller{
				PubsubClient:  c,
				ForwardClient: h.ForwardClient,
				CancelFunc:    cancel,
				Listener:      listener,
				ErrCh:         make(chan error),
			}
			h.pullersMap.Store(id, p)

			// How to handle?
			h.Logger.Infof("started receiving events from purgatory subscription: %s", listener.PurgatorySubscription)
			go func() {
				p.ErrCh <- p.PubsubClient.StartReceiver(ctx, p.receive)
			}()
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("failed to reconcile pullers: %v", errs)
	}

	h.pullersMap.Range(func(key, value interface{}) bool {
		id := key.(string)
		p := value.(*Puller)
		if _, ok := listeners[id]; !ok {
			p.CancelFunc()
			select {
			case <-p.ErrCh:
				h.Logger.Info("Gracefully shutdown subscription pull")
			case <-time.After(time.Minute):
				h.Logger.Error("Failed to shutdown subscription pull within grace period")
			}
			h.pullersMap.Delete(id)
		}
		return true
	})
	return nil
}

func (p *Puller) receive(ctx context.Context, event cloudevents.Event, _ *cloudevents.EventResponse) error {
	if utils.PassFilter(p.Listener.Filters, &event) {
		tctx := cloudevents.HTTPTransportContextFrom(ctx)
		_, _, err := p.ForwardClient.Send(utils.SendingContextFrom(ctx, tctx, p.Listener.Target), event)
		if err != nil {
			return fmt.Errorf("failed to forward event %s: %w", event.ID(), err)
		}
	}
	return nil
}
