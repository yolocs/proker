package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	cloudevents "github.com/cloudevents/sdk-go"
	"github.com/google/uuid"
	"github.com/kelseyhightower/envconfig"
)

type flags struct {
	Target   string `envconfig:"TARGET" required:"true"`
	Interval int    `envconfig:"INTERVAL" required:"true"`
}

// func main() {
// 	// Parse the environment.
// 	var env flags
// 	if err := envconfig.Process("", &env); err != nil {
// 		fmt.Fprintln(os.Stderr, err)
// 		os.Exit(1)
// 	}

// 	tOpts := []cepubsub.Option{
// 		cepubsub.WithProjectID(env.Project),
// 		cepubsub.WithTopicID(env.Topic),
// 		cepubsub.WithSubscriptionAndTopicID(env.Subscription, env.Topic),
// 	}
// 	t, err := cepubsub.New(context.Background(), tOpts...)
// 	if err != nil {
// 		fmt.Fprintln(os.Stderr, err)
// 		os.Exit(1)
// 	}
// 	c, err := cloudevents.NewClient(t)
// 	if err != nil {
// 		fmt.Fprintln(os.Stderr, err)
// 		os.Exit(1)
// 	}

// 	for {
// 		event := cloudevents.NewEvent(cloudevents.VersionV1)
// 		event.SetSource("proker-seender")
// 		event.SetID(uuid.New().String())
// 		event.SetSubject("seeding")
// 		event.SetType("seeding")
// 		event.SetData("seeding data")

// 		log.Println("seending event", event.ID())
// 		_, _, err := c.Send(context.Background(), event)
// 		if err != nil {
// 			log.Println("failed to seed event", event.ID(), err)
// 		}

// 		<-time.After(time.Duration(env.Interval) * time.Second)
// 	}
// }

func main() {
	// Parse the environment.
	var env flags
	if err := envconfig.Process("", &env); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	c, err := newCeClinet(env.Target)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	for {
		event := cloudevents.NewEvent(cloudevents.VersionV1)
		event.SetSource("proker-seender")
		event.SetID(uuid.New().String())
		event.SetSubject("seeding")
		event.SetType("greeting")

		log.Println("seending event", event.ID())
		_, _, err := c.Send(context.Background(), event)
		if err != nil {
			log.Println("failed to seed event", event.ID(), err)
		}

		<-time.After(time.Duration(env.Interval) * time.Second)
	}
}

func newCeClinet(target string) (cloudevents.Client, error) {
	base := http.DefaultTransport
	baseTransport := base.(*http.Transport)
	baseTransport.MaxIdleConns = 1000
	baseTransport.MaxIdleConnsPerHost = 100

	t, err := cloudevents.NewHTTPTransport(cloudevents.WithBinaryEncoding(), cloudevents.WithTarget(target))
	if err != nil {
		return nil, fmt.Errorf("failed to create ce http transport: %w", err)
	}
	t.Client = &http.Client{
		Transport: base,
	}

	return cloudevents.NewClient(t, cloudevents.WithUUIDs(), cloudevents.WithTimeNow())
}
