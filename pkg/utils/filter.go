package utils

import (
	cloudevents "github.com/cloudevents/sdk-go"
)

func PassFilter(attrs map[string]string, event *cloudevents.Event) bool {
	// Set standard context attributes. The attributes available may not be
	// exactly the same as the attributes defined in the current version of the
	// CloudEvents spec.
	ce := map[string]interface{}{
		"specversion":     event.SpecVersion(),
		"type":            event.Type(),
		"source":          event.Source(),
		"subject":         event.Subject(),
		"id":              event.ID(),
		"time":            event.Time().String(),
		"schemaurl":       event.DataSchema(),
		"datacontenttype": event.DataContentType(),
		"datamediatype":   event.DataMediaType(),
		// TODO: use data_base64 when SDK supports it.
		"datacontentencoding": event.DeprecatedDataContentEncoding(),
	}
	ext := event.Extensions()
	if ext != nil {
		for k, v := range ext {
			ce[k] = v
		}
	}

	for k, v := range attrs {
		var value interface{}
		value, ok := ce[k]
		// If the attribute does not exist in the event, return false.
		if !ok {
			return false
		}
		// If the attribute is not set to any and is different than the one from the event, return false.
		if v != "*" && v != value {
			return false
		}
	}
	return true
}
