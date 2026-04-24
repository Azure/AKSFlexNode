package apply

import (
	"bytes"
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/Azure/AKSFlexNode/components/api"
)

// isJSONContent reports whether input appears to be a JSON object or array.
// It inspects the first non-whitespace byte: JSON objects begin with '{' and
// JSON arrays begin with '['; binary protobuf never starts with either byte.
func isJSONContent(input []byte) bool {
	for _, b := range input {
		if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
			continue
		}
		return b == '{' || b == '['
	}
	return false
}

// parseActions detects the input format and returns the pre-parsed actions.
func parseActions(input []byte) ([]parsedAction, error) {
	if isJSONContent(input) {
		return parseActionFromJSON(input)
	}

	pa, err := parseActionFromProto(input)
	if err != nil {
		return nil, err
	}
	return []parsedAction{pa}, nil
}

// parseActionFromJSON deserializes one or more JSON-encoded actions. The input
// may be a single JSON object or a JSON array of objects.
func parseActionFromJSON(input []byte) ([]parsedAction, error) {
	tok, err := json.NewDecoder(bytes.NewBuffer(input)).Token()
	if err != nil {
		return nil, err
	}

	var bs []json.RawMessage
	if tok == json.Delim('[') {
		if err := json.Unmarshal(input, &bs); err != nil {
			return nil, err
		}
	} else {
		bs = append(bs, input)
	}

	// Pre-parse all actions so we know the total count and names up front.
	parsed := make([]parsedAction, 0, len(bs))
	for _, b := range bs {
		pa, err := parseAction(b)
		if err != nil {
			return nil, err
		}
		parsed = append(parsed, pa)
	}
	return parsed, nil
}

func parseAction(b []byte) (parsedAction, error) {
	base := &api.Base{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, base); err != nil {
		return parsedAction{}, err
	}

	actionType := base.GetMetadata().GetType()
	actionName := base.GetMetadata().GetName()

	mt, err := protoregistry.GlobalTypes.FindMessageByURL(actionType)
	if err != nil {
		return parsedAction{}, fmt.Errorf("lookup action type %q: %w", actionType, err)
	}

	m := mt.New().Interface()
	if err := protojson.Unmarshal(b, m); err != nil {
		return parsedAction{}, fmt.Errorf("unmarshal action %q: %w", actionType, err)
	}

	// Use the action name if available, otherwise fall back to the type URL.
	name := actionName
	if name == "" {
		name = actionType
	}

	return parsedAction{name: name, message: m}, nil
}

// parseActionFromProto deserializes a single binary protobuf-encoded action.
func parseActionFromProto(b []byte) (parsedAction, error) {
	base := &api.Base{}
	if err := proto.Unmarshal(b, base); err != nil {
		return parsedAction{}, err
	}

	actionType := base.GetMetadata().GetType()
	actionName := base.GetMetadata().GetName()

	mt, err := protoregistry.GlobalTypes.FindMessageByURL(actionType)
	if err != nil {
		return parsedAction{}, fmt.Errorf("lookup action type %q: %w", actionType, err)
	}

	m := mt.New().Interface()
	if err := proto.Unmarshal(b, m); err != nil {
		return parsedAction{}, fmt.Errorf("unmarshal action %q: %w", actionType, err)
	}

	// Use the action name if available, otherwise fall back to the type URL.
	name := actionName
	if name == "" {
		name = actionType
	}

	return parsedAction{name: name, message: m}, nil
}
