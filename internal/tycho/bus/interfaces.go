package bus

import "context"

// Event carries both event and arrival timestamps (monotonic/wall later).
type Event struct {
	Topic string
	Data  any
	// TODO: eventTs, arrivalTs
}

// EventBus defines a minimal pub/sub we can implement later.
type EventBus interface {
	Publish(ctx context.Context, e Event) error
	Subscribe(ctx context.Context, topic string) (<-chan Event, func(), error) // returns channel + unsubscribe
	Close() error
}
