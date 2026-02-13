package apply

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"go.goms.io/aks/AKSFlexNode/pkg/actions"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

const stdinFilePath = "-"

var flagActionFilePath string

var Command = &cobra.Command{
	Use:  "apply",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		var input []byte
		var err error
		if flagActionFilePath == stdinFilePath {
			input, err = io.ReadAll(os.Stdin)
		} else {
			input, err = os.ReadFile(flagActionFilePath)
		}
		if err != nil {
			return err
		}

		return apply(cmd.Context(), input)
	},
	SilenceUsage: true,
}

func init() {
	Command.Flags().StringVarP(
		&flagActionFilePath,
		"filename", "f", stdinFilePath,
		"Path to the action file to apply. Use '-' to read from stdin.",
	)
	Command.MarkFlagRequired("filename")
}

func apply(ctx context.Context, input []byte) error {
	tok, err := json.NewDecoder(bytes.NewBuffer(input)).Token()
	if err != nil {
		return err
	}

	var bs []json.RawMessage
	if tok == json.Delim('[') {
		if err := json.Unmarshal(input, &bs); err != nil {
			return err
		}
	} else {
		bs = append(bs, input)
	}

	for _, b := range bs {
		if err := applyOne(ctx, b); err != nil {
			return err
		}
	}

	return nil
}

func applyOne(ctx context.Context, b []byte) error {
	action := &actions.Base{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, action); err != nil {
		return err
	}

	actionType := action.GetMetadata().GetType()

	protoregistry.GlobalTypes.RangeMessages(func(mt protoreflect.MessageType) bool {
		fmt.Println(mt.Descriptor().FullName())

		return true
	})

	mt, err := protoregistry.GlobalTypes.FindMessageByURL(actionType)
	if err != nil {
		return fmt.Errorf("lookup action type %q: %w", actionType, err)
	}

	m := mt.New().Interface()
	if err := protojson.Unmarshal(b, m); err != nil {
		return fmt.Errorf("unmarshal action %q: %w", actionType, err)
	}

	fmt.Println("Applying action:", m)

	return nil
}
