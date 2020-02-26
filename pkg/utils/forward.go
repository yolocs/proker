package utils

import (
	"context"
	"net/http"
	"strings"

	cloudevents "github.com/cloudevents/sdk-go"
	"k8s.io/apimachinery/pkg/util/sets"
)

var (
	// These MUST be lowercase strings, as they will be compared against lowercase strings.
	forwardHeaders = sets.NewString(
		// tracing
		"x-request-id",
	)
	// These MUST be lowercase strings, as they will be compared against lowercase strings.
	// Removing CloudEvents ce- prefixes on purpose as they should be set in the CloudEvent itself as extensions.
	// Then the SDK will set them as ce- headers when sending them through HTTP. Otherwise, when using replies we would
	// duplicate ce- headers.
	forwardPrefixes = []string{
		// knative
		"knative-",
	}
)

func SendingContextFrom(ctx context.Context, tctx cloudevents.HTTPTransportContext, targetURI string) context.Context {
	// Get the allowed set of headers.
	h := passThroughHeaders(tctx.Header)
	for k, v := range h {
		for _, vv := range v {
			ctx = cloudevents.ContextWithHeader(ctx, k, vv)
		}
	}
	if targetURI != "" {
		ctx = cloudevents.ContextWithTarget(ctx, targetURI)
	}

	return ctx
}

func passThroughHeaders(headers http.Header) http.Header {
	h := http.Header{}

	for n, v := range headers {
		lower := strings.ToLower(n)
		if forwardHeaders.Has(lower) {
			h[n] = v
			continue
		}
		for _, prefix := range forwardPrefixes {
			if strings.HasPrefix(lower, prefix) {
				h[n] = v
				break
			}
		}
	}
	return h
}
