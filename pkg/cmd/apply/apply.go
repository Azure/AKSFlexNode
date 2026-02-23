package apply

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"time"

	"github.com/schollz/progressbar/v3"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoregistry"

	"go.goms.io/aks/AKSFlexNode/components/api"
	"go.goms.io/aks/AKSFlexNode/components/services/actions"
	"go.goms.io/aks/AKSFlexNode/components/services/inmem"
)

const stdinFilePath = "-"

var (
	flagActionFilePath string
	flagNoPrettyUI     bool
)

var Command = &cobra.Command{
	Use:  "apply",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		var input []byte
		var err error
		if flagActionFilePath == stdinFilePath {
			input, err = io.ReadAll(os.Stdin)
		} else {
			input, err = os.ReadFile(path.Clean(flagActionFilePath))
		}
		if err != nil {
			return err
		}

		return apply(cmd.Context(), input, flagNoPrettyUI)
	},
	SilenceUsage: true,
}

func init() {
	Command.Flags().StringVarP(
		&flagActionFilePath,
		"filename", "f", stdinFilePath,
		"Path to the action file to apply. Use '-' to read from stdin.",
	)
	_ = Command.MarkFlagRequired("filename") //nolint: errcheck // flag setup
	Command.Flags().BoolVar(
		&flagNoPrettyUI,
		"no-prettyui", false,
		"Disable progress bar and colored summary output.",
	)
}

// parsedAction holds a pre-parsed action ready to be applied.
type parsedAction struct {
	name    string
	message proto.Message
}

// stepResult records the outcome and duration of a single action.
type stepResult struct {
	name     string
	duration time.Duration
	err      error
}

func apply(ctx context.Context, input []byte, noPrettyUI bool) error {
	conn, err := inmem.NewConnection()
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck // close connection

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

	// Pre-parse all actions so we know the total count and names up front.
	parsed := make([]parsedAction, 0, len(bs))
	for _, b := range bs {
		pa, err := parseAction(b)
		if err != nil {
			return err
		}
		parsed = append(parsed, pa)
	}

	if noPrettyUI {
		return applyPlain(ctx, conn, parsed)
	}
	return applyPretty(ctx, conn, parsed)
}

// applyPlain executes actions with no progress bar or colors.
func applyPlain(ctx context.Context, conn *grpc.ClientConn, parsed []parsedAction) error {
	for i, pa := range parsed {
		fmt.Fprintf(os.Stderr, "[%d/%d] applying %s\n", i+1, len(parsed), pa.name) // #nosec G705

		start := time.Now()
		_, err := actions.ApplyAction(conn, ctx, pa.message)
		elapsed := time.Since(start)

		if err != nil {
			fmt.Fprintf(os.Stderr, "[%d/%d] %s failed (%s): %v\n", i+1, len(parsed), pa.name, formatDuration(elapsed), err) // #nosec G705
			return err
		}
		fmt.Fprintf(os.Stderr, "[%d/%d] %s done (%s)\n", i+1, len(parsed), pa.name, formatDuration(elapsed)) // #nosec G705
	}
	return nil
}

// applyPretty executes actions with a progress bar and colored summary.
func applyPretty(ctx context.Context, conn *grpc.ClientConn, parsed []parsedAction) error {
	bar := progressbar.NewOptions(-1,
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionSetDescription("starting"),
		progressbar.OptionSetWidth(30),
		progressbar.OptionClearOnFinish(),
		progressbar.OptionSetElapsedTime(false),
		progressbar.OptionSetPredictTime(false),
		progressbar.OptionSpinnerType(11),
		progressbar.OptionSetSpinnerChangeInterval(100*time.Millisecond),
		progressbar.OptionThrottle(60*time.Millisecond),
		progressbar.OptionEnableColorCodes(true),
		progressbar.OptionSetRenderBlankState(true),
	)

	results := make([]stepResult, 0, len(parsed))
	var applyErr error

	for i, pa := range parsed {
		bar.Describe(fmt.Sprintf("[%d/%d] applying %s", i+1, len(parsed), pa.name))

		start := time.Now()
		_, err := actions.ApplyAction(conn, ctx, pa.message)
		elapsed := time.Since(start)

		results = append(results, stepResult{name: pa.name, duration: elapsed, err: err})

		if err != nil {
			_ = bar.Exit()
			applyErr = err
			break
		}
		_ = bar.Add(1)
	}

	_ = bar.Finish()
	printSummary(os.Stderr, results)

	return applyErr
}

// ANSI color helpers.
const (
	colorReset = "\033[0m"
	colorRed   = "\033[31m"
	colorGreen = "\033[32m"
	colorCyan  = "\033[36m"
	colorDim   = "\033[2m"
	colorBold  = "\033[1m"
)

// printSummary writes a color-coded table of step names, durations, and pass/fail status.
func printSummary(w io.Writer, results []stepResult) {
	if len(results) == 0 {
		return
	}

	// Find the longest name for alignment.
	maxName := 0
	for _, r := range results {
		if len(r.name) > maxName {
			maxName = len(r.name)
		}
	}

	fmt.Fprintln(w)
	var total time.Duration
	passed, failed := 0, 0
	for _, r := range results {
		total += r.duration

		name := fmt.Sprintf("%-*s", maxName, r.name)
		dur := fmt.Sprintf("%8s", formatDuration(r.duration))

		if r.err != nil {
			failed++
			fmt.Fprintf(w, "  %s%s%s  %s%s%s  %s%sFAILED:%s %v\n",
				colorBold, name, colorReset,
				colorDim, dur, colorReset,
				colorRed, colorBold, colorReset, r.err)
		} else {
			passed++
			fmt.Fprintf(w, "  %s%s%s  %s%s%s  %s%sok%s\n",
				colorCyan, name, colorReset,
				colorDim, dur, colorReset,
				colorGreen, colorBold, colorReset)
		}
	}

	fmt.Fprintln(w)
	summary := fmt.Sprintf("  %d action(s) in %s", len(results), formatDuration(total))
	if failed > 0 {
		fmt.Fprintf(w, "%s%s%s (%s%d passed%s, %s%d failed%s)\n",
			colorBold, summary, colorReset,
			colorGreen, passed, colorReset,
			colorRed, failed, colorReset)
	} else {
		fmt.Fprintf(w, "%s%s%s — %sall passed%s\n",
			colorGreen, summary, colorReset,
			colorGreen, colorReset)
	}
}

func formatDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	default:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
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
