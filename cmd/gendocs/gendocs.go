// Package gendocs provides the gendocs command.
package gendocs

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/artpar/rclone/cmd"
	"github.com/artpar/rclone/fs/config/flags"
	"github.com/artpar/rclone/lib/file"
	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
)

func init() {
	cmd.Root.AddCommand(commandDefinition)
}

// define things which go into the frontmatter
type frontmatter struct {
	Date        string
	Title       string
	Description string
	Slug        string
	URL         string
	Source      string
	Annotations map[string]string
}

var frontmatterTemplate = template.Must(template.New("frontmatter").Parse(`---
title: "{{ .Title }}"
description: "{{ .Description }}"
slug: {{ .Slug }}
url: {{ .URL }}
{{- range $key, $value := .Annotations }}
{{ $key }}: {{  $value }}
{{- end }}
# autogenerated - DO NOT EDIT, instead edit the source code in {{ .Source }} and as part of making a release run "make commanddocs"
---
`))

var commandDefinition = &cobra.Command{
	Use:   "gendocs output_directory",
	Short: `Output markdown docs for rclone to the directory supplied.`,
	Long: `
This produces markdown docs for the rclone commands to the directory
supplied.  These are in a format suitable for hugo to render into the
rclone.org website.`,
	Annotations: map[string]string{
		"versionIntroduced": "v1.33",
	},
	RunE: func(command *cobra.Command, args []string) error {
		cmd.CheckArgs(1, 1, command, args)
		now := time.Now().Format(time.RFC3339)

		// Create the directory structure
		root := args[0]
		out := filepath.Join(root, "commands")
		err := file.MkdirAll(out, 0777)
		if err != nil {
			return err
		}

		// Write the flags page
		var buf bytes.Buffer
		cmd.Root.SetOutput(&buf)
		cmd.Root.SetArgs([]string{"help", "flags"})
		cmd.GeneratingDocs = true
		err = cmd.Root.Execute()
		if err != nil {
			return err
		}
		err = os.WriteFile(filepath.Join(root, "flags.md"), buf.Bytes(), 0777)
		if err != nil {
			return err
		}

		// Look up name => details for prepender
		type commandDetails struct {
			Short       string
			Annotations map[string]string
		}
		var commands = map[string]commandDetails{}
		var aliases []string
		var addCommandDetails func(root *cobra.Command)
		addCommandDetails = func(root *cobra.Command) {
			name := strings.ReplaceAll(root.CommandPath(), " ", "_") + ".md"
			commands[name] = commandDetails{
				Short:       root.Short,
				Annotations: root.Annotations,
			}
			aliases = append(aliases, root.Aliases...)
			for _, c := range root.Commands() {
				addCommandDetails(c)
			}
		}
		addCommandDetails(cmd.Root)

		// markup for the docs files
		prepender := func(filename string) string {
			name := filepath.Base(filename)
			base := strings.TrimSuffix(name, path.Ext(name))
			data := frontmatter{
				Date:        now,
				Title:       strings.ReplaceAll(base, "_", " "),
				Description: commands[name].Short,
				Slug:        base,
				URL:         "/commands/" + strings.ToLower(base) + "/",
				Source:      strings.ReplaceAll(strings.ReplaceAll(base, "rclone", "cmd"), "_", "/") + "/",
				Annotations: commands[name].Annotations,
			}
			var buf bytes.Buffer
			err := frontmatterTemplate.Execute(&buf, data)
			if err != nil {
				log.Fatalf("Failed to render frontmatter template: %v", err)
			}
			return buf.String()
		}
		linkHandler := func(name string) string {
			base := strings.TrimSuffix(name, path.Ext(name))
			return "/commands/" + strings.ToLower(base) + "/"
		}

		err = doc.GenMarkdownTreeCustom(cmd.Root, out, prepender, linkHandler)
		if err != nil {
			return err
		}

		var outdentTitle = regexp.MustCompile(`(?m)^#(#+)`)

		// Munge the files to add a link to the global flags page
		err = filepath.Walk(out, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() {
				name := filepath.Base(path)
				cmd, ok := commands[name]
				if !ok {
					// Avoid man pages which are for aliases. This is a bit messy!
					for _, alias := range aliases {
						if strings.Contains(name, alias) {
							return nil
						}
					}
					return fmt.Errorf("didn't find command for %q", name)
				}
				b, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				doc := string(b)

				var out strings.Builder
				if groupsString := cmd.Annotations["groups"]; groupsString != "" {
					groups := flags.All.Include(groupsString)
					for _, group := range groups.Groups {
						if group.Flags.HasFlags() {
							_, _ = fmt.Fprintf(&out, "\n### %s Options\n\n", group.Name)
							_, _ = fmt.Fprintf(&out, "%s\n\n", group.Help)
							_, _ = fmt.Fprintln(&out, "```")
							_, _ = out.WriteString(group.Flags.FlagUsages())
							_, _ = fmt.Fprintln(&out, "```")
						}
					}
				}
				_, _ = out.WriteString(`
See the [global flags page](/flags/) for global options not listed here.

`)

				startCut := strings.Index(doc, `### Options inherited from parent commands`)
				endCut := strings.Index(doc, `## SEE ALSO`)
				if startCut < 0 || endCut < 0 {
					if name == "rclone.md" {
						return nil
					}
					return fmt.Errorf("internal error: failed to find cut points: startCut = %d, endCut = %d", startCut, endCut)
				}
				doc = doc[:startCut] + out.String() + doc[endCut:]
				// outdent all the titles by one
				doc = outdentTitle.ReplaceAllString(doc, `$1`)
				err = os.WriteFile(path, []byte(doc), 0777)
				if err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}

		return nil
	},
}
