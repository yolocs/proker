package fanout

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	cloudevents "github.com/cloudevents/sdk-go"
	cepubsub "github.com/cloudevents/sdk-go/pkg/cloudevents/transport/pubsub"
	"github.com/micro/go-micro/config"
	"github.com/yolocs/proker/pkg/utils"
	"go.uber.org/zap"
)

type PubsubConfig struct {
	ProjectID, TopicID, SubscriptionID, PurgatoryTopicID string
}

type ForwardResult struct {
	Err         error
	IsPurgatory bool
	IsFiltered  bool
}

type Handler struct {
	Logger                 *zap.SugaredLogger
	ListenersCfg           config.Config
	PubsubCfg              *PubsubConfig
	MaxConcurrencyPerEvent int
	ForwardClient          cloudevents.Client

	mainPubsub      cloudevents.Client
	purgatoryPubsub cloudevents.Client
}

func (h *Handler) Start(ctx context.Context) error {
	ptOpts := []cepubsub.Option{
		cepubsub.WithProjectID(h.PubsubCfg.ProjectID),
		cepubsub.WithTopicID(h.PubsubCfg.PurgatoryTopicID),
	}
	pt, err := cepubsub.New(ctx, ptOpts...)
	if err != nil {
		return fmt.Errorf("failed to create cloudevents pubsub transport: %w", err)
	}
	pc, err := cloudevents.NewClient(pt)
	if err != nil {
		return fmt.Errorf("failed to create cloudevents client: %w", err)
	}
	h.purgatoryPubsub = pc

	tOpts := []cepubsub.Option{
		cepubsub.WithProjectID(h.PubsubCfg.ProjectID),
		cepubsub.WithTopicID(h.PubsubCfg.TopicID),
		cepubsub.WithSubscriptionAndTopicID(h.PubsubCfg.SubscriptionID, h.PubsubCfg.TopicID),
	}
	t, err := cepubsub.New(ctx, tOpts...)
	if err != nil {
		return fmt.Errorf("failed to create cloudevents pubsub transport: %w", err)
	}

	c, err := cloudevents.NewClient(t)
	if err != nil {
		return fmt.Errorf("failed to create cloudevents client: %w", err)
	}
	h.mainPubsub = c
	return h.mainPubsub.StartReceiver(ctx, h.receive)
}

func (h *Handler) receive(ctx context.Context, event cloudevents.Event, _ *cloudevents.EventResponse) error {
	var listeners map[string]utils.Listener
	if err := h.ListenersCfg.Scan(&listeners); err != nil {
		return fmt.Errorf("failed to decode listener config: %w", err)
	}

	lc := make(chan utils.Listener)
	go func() {
		defer close(lc)
		for _, listener := range listeners {
			lc <- listener
		}
	}()

	cc := h.MaxConcurrencyPerEvent
	if cc > len(listeners) {
		cc = len(listeners)
	}

	h.Logger.Infof("faning out event %s with %d channels", event.ID(), cc)

	var cs []<-chan ForwardResult
	for i := 0; i < cc; i++ {
		cs = append(cs, h.forwardEvent(ctx, event, lc))
	}

	return h.mergeResult(ctx, cs)
}

func (h *Handler) forwardEvent(ctx context.Context, event cloudevents.Event, lc <-chan utils.Listener) <-chan ForwardResult {
	tctx := cloudevents.HTTPTransportContextFrom(ctx)
	out := make(chan ForwardResult)
	go func() {
		defer close(out)
		for listener := range lc {
			if utils.PassFilter(listener.Filters, &event) {
				_, _, err := h.ForwardClient.Send(utils.SendingContextFrom(ctx, tctx, listener.Target), event)
				// rtctx := cloudevents.HTTPTransportContextFrom(rctx)
				// h.Logger.Infof("Fanout status code %d", rtctx.StatusCode)
				if err == nil {
					out <- ForwardResult{}
				} else {
					_, _, err := h.purgatoryPubsub.Send(utils.SendingContextFrom(ctx, tctx, ""), event)
					if err == nil {
						out <- ForwardResult{IsPurgatory: true}
					} else {
						out <- ForwardResult{Err: err}
					}
				}
			} else {
				out <- ForwardResult{IsFiltered: true}
			}
		}
	}()
	return out
}

func (h *Handler) mergeResult(ctx context.Context, cs []<-chan ForwardResult) error {
	var wg sync.WaitGroup
	var passed, failed, filtered, retrying int64

	countResult := func(c <-chan ForwardResult) {
		for fr := range c {
			if fr.Err != nil {
				atomic.AddInt64(&failed, 1)
			} else if fr.IsPurgatory {
				atomic.AddInt64(&retrying, 1)
			} else if fr.IsFiltered {
				atomic.AddInt64(&filtered, 1)
			} else {
				atomic.AddInt64(&passed, 1)
			}
		}
		wg.Done()
	}

	wg.Add(len(cs))
	for _, c := range cs {
		go countResult(c)
	}

	wg.Wait()

	h.Logger.Infof("Successfully faned out to %d listeners, failed %d, filtered %d, retrying %d", passed, failed, filtered, retrying)

	if failed > 0 {
		return fmt.Errorf("failed to deliver %d events", failed)
	}
	return nil
}
