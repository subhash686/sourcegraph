package docgen

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/template"

	"github.com/urfave/cli/v2"
)

// Markdown is adapted from https://sourcegraph.com/github.com/urfave/cli@v2.4.0/-/blob/docs.go?L16
func Markdown(app *cli.App) (string, error) {
	var w bytes.Buffer
	if err := writeDocTemplate(app, &w); err != nil {
		return "", err
	}
	return w.String(), nil
}

type cliTemplate struct {
	App        *cli.App
	Commands   []string
	GlobalArgs []string
}

func writeDocTemplate(app *cli.App, w io.Writer) error {
	const name = "cli"
	t, err := template.New(name).Parse(markdownDocTemplate)
	if err != nil {
		return err
	}
	return t.ExecuteTemplate(w, name, &cliTemplate{
		App:        app,
		Commands:   prepareCommands(app.Name, app.Commands, 0),
		GlobalArgs: prepareArgsWithValues(app.VisibleFlags()),
	})
}

func prepareCommands(lineage string, commands []*cli.Command, level int) []string {
	var coms []string
	for _, command := range commands {
		if command.Hidden {
			continue
		}

		usageText := prepareUsageText(command)

		usage := prepareUsage(command, usageText)

		prepared := fmt.Sprintf("%s %s\n\n%s%s",
			strings.Repeat("#", level+2),
			lineage+" "+command.Names()[0],
			usage,
			usageText,
		)

		flags := prepareArgsWithValues(command.Flags)
		if len(flags) > 0 {
			prepared += fmt.Sprintf("\n%s", strings.Join(flags, "\n"))
		}

		if len(command.Description) > 0 {
			prepared += fmt.Sprintf("\n%s", command.Description)
		}

		coms = append(coms, prepared)

		// recursevly iterate subcommands
		if len(command.Subcommands) > 0 {
			coms = append(
				coms,
				prepareCommands(lineage+" "+command.Names()[0], command.Subcommands, level+1)...,
			)
		}
	}

	return coms
}

func prepareArgsWithValues(flags []cli.Flag) []string {
	return prepareFlags(flags, ", ", "**", "**", `""`, true)
}

func prepareFlags(
	flags []cli.Flag,
	sep, opener, closer, value string,
	addDetails bool,
) []string {
	args := []string{}
	for _, f := range flags {
		flag, ok := f.(cli.DocGenerationFlag)
		if !ok {
			continue
		}
		modifiedArg := opener

		for _, s := range flag.Names() {
			trimmed := strings.TrimSpace(s)
			if len(modifiedArg) > len(opener) {
				modifiedArg += sep
			}
			if len(trimmed) > 1 {
				modifiedArg += fmt.Sprintf("--%s", trimmed)
			} else {
				modifiedArg += fmt.Sprintf("-%s", trimmed)
			}
		}
		modifiedArg += closer
		if flag.TakesValue() {
			modifiedArg += fmt.Sprintf("=%s", value)
		}

		if addDetails {
			modifiedArg += flagDetails(flag)
		}

		args = append(args, modifiedArg+"\n")

	}
	sort.Strings(args)
	return args
}

// flagDetails returns a string containing the flags metadata
func flagDetails(flag cli.DocGenerationFlag) string {
	description := flag.GetUsage()
	value := flag.GetValue()
	if value != "" {
		description += " (default: " + value + ")"
	}
	return ": " + description
}

func prepareUsageText(command *cli.Command) string {
	if command.UsageText == "" {
		if strings.TrimSpace(command.ArgsUsage) != "" {
			return fmt.Sprintf("**Arguments:** `%s`\n", command.ArgsUsage)
		}
		return ""
	}

	// Remove leading and trailing newlines
	preparedUsageText := strings.Trim(command.UsageText, "\n")

	var usageText string
	if strings.Contains(preparedUsageText, "\n") {
		usageText += "```sh"
		for _, ln := range strings.Split(preparedUsageText, "\n") {
			usageText += fmt.Sprintf("\n$ %s", ln)
		}
		usageText += "```"
	} else {
		// Style a single line as a note
		usageText = fmt.Sprintf("```sh\n$ %s\n```\n", preparedUsageText)
	}

	return usageText
}

func prepareUsage(command *cli.Command, usageText string) string {
	if command.Usage == "" {
		return ""
	}

	usage := command.Usage + ".\n"
	// Add a newline to the Usage IFF there is a UsageText
	if usageText != "" {
		usage += "\n"
	}

	return usage
}

var markdownDocTemplate = `# NAME

{{ .App.Name }}{{ if .App.Usage }} - {{ .App.Usage }}{{ end }}
{{ if .App.Description }}
{{ .App.Description }}
{{ end }}
**Usage**:

` + "```" + `{{ if .App.UsageText }}
{{ .App.UsageText }}
{{ else }}
{{ .App.Name }} [GLOBAL OPTIONS] command [COMMAND OPTIONS] [ARGUMENTS...]
{{ end }}` + "```" + `
{{ if .GlobalArgs }}
# GLOBAL OPTIONS
{{ range $v := .GlobalArgs }}
{{ $v }}{{ end }}
{{ end }}{{ if .Commands }}
# COMMANDS
{{ range $v := .Commands }}
{{ $v }}{{ end }}{{ end }}`
