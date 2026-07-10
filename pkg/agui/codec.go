package agui

import (
	"encoding/json"
	"fmt"
)

// EncodeEvent serializes an AG-UI event to JSON. The concrete event struct must
// already carry its discriminator via the embedded BaseEvent (constructors set
// this); EncodeEvent does not mutate the event.
func EncodeEvent(e Event) ([]byte, error) {
	if e.EventType() == "" {
		return nil, fmt.Errorf("agui: event has empty type")
	}
	data, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("agui: encode event: %w", err)
	}
	return data, nil
}

// DecodeEvent parses a JSON AG-UI event, dispatching on its "type" field to the
// matching concrete struct. The returned Event is a pointer to that struct.
func DecodeEvent(data []byte) (Event, error) {
	var head struct {
		Type EventType `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("agui: decode event type: %w", err)
	}
	if head.Type == "" {
		return nil, fmt.Errorf("agui: event missing type field")
	}
	proto, err := eventPrototype(head.Type)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, proto); err != nil {
		return nil, fmt.Errorf("agui: decode %s: %w", head.Type, err)
	}
	return proto, nil
}
