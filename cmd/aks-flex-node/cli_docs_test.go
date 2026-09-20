package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	cliReferenceBegin = "<!-- BEGIN GENERATED CLI REFERENCE -->"
	cliReferenceEnd   = "<!-- END GENERATED CLI REFERENCE -->"
	requiredFlagKey   = "cobra_annotation_bash_completion_one_required_flag"
)

func TestCLIReferenceIsCurrent(t *testing.T) {
	docPath := cliReferencePath(t)
	current, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read CLI reference: %v", err)
	}

	want := replaceGeneratedCLIReference(t, string(current), renderCLIReference(newRootCommand()))
	if os.Getenv("UPDATE_CLI_DOCS") == "1" {
		if err := os.WriteFile(docPath, []byte(want), 0o644); err != nil {
			t.Fatalf("write CLI reference: %v", err)
		}
		return
	}

	if got := string(current); got != want {
		t.Error("docs/usage/cli.md is out of date; run make docs-cli-generate")
	}
}

func cliReferencePath(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", "docs", "usage", "cli.md"))
}

func replaceGeneratedCLIReference(t *testing.T, document, generated string) string {
	t.Helper()
	begin := strings.Index(document, cliReferenceBegin)
	end := strings.Index(document, cliReferenceEnd)
	if begin < 0 || end < 0 || end < begin {
		t.Fatalf("CLI reference markers are missing or out of order")
	}
	begin += len(cliReferenceBegin)
	return document[:begin] + "\n" + generated + document[end:]
}

func renderCLIReference(root *cobra.Command) string {
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()

	commands := collectCLICommands(root)
	var output strings.Builder
	output.WriteString("<!-- Run make docs-cli-generate; do not edit this section manually. -->\n\n")
	output.WriteString("| Usage | Description | Aliases | Cobra visibility | Local flags |\n")
	output.WriteString("| --- | --- | --- | --- | --- |\n")
	for _, command := range commands {
		fmt.Fprintf(
			&output,
			"| `%s` | %s | %s | %s | %s |\n",
			markdownCell(command.UseLine()),
			markdownCell(command.Short),
			renderAliases(command.Aliases),
			renderVisibility(command),
			renderFlags(command),
		)
	}
	return output.String()
}

func collectCLICommands(root *cobra.Command) []*cobra.Command {
	var commands []*cobra.Command
	var visit func(*cobra.Command)
	visit = func(parent *cobra.Command) {
		children := append([]*cobra.Command(nil), parent.Commands()...)
		sort.Slice(children, func(i, j int) bool {
			return children[i].CommandPath() < children[j].CommandPath()
		})
		for _, child := range children {
			commands = append(commands, child)
			visit(child)
		}
	}
	visit(root)
	return commands
}

func renderAliases(aliases []string) string {
	if len(aliases) == 0 {
		return "—"
	}
	values := append([]string(nil), aliases...)
	sort.Strings(values)
	for i := range values {
		values[i] = "`" + markdownCell(values[i]) + "`"
	}
	return strings.Join(values, ", ")
}

func renderVisibility(command *cobra.Command) string {
	if command.Hidden {
		return "hidden"
	}
	return "listed"
}

func renderFlags(command *cobra.Command) string {
	command.InitDefaultHelpFlag()
	var values []string
	command.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
		if f.Name == "help" {
			return
		}
		value := "`--" + markdownCell(f.Name) + "`"
		if f.Shorthand != "" {
			value += ", `-" + markdownCell(f.Shorthand) + "`"
		}
		if _, required := f.Annotations[requiredFlagKey]; required {
			value += " (required)"
		} else if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "[]" {
			value += " (default: `" + markdownCell(f.DefValue) + "`)"
		}
		values = append(values, value)
	})
	if len(values) == 0 {
		return "—"
	}
	sort.Strings(values)
	return strings.Join(values, "; ")
}

func markdownCell(value string) string {
	return strings.ReplaceAll(value, "|", "\\|")
}
