package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/artpar/rclone/fs"
	"github.com/artpar/rclone/fs/config/configflags"
	"github.com/artpar/rclone/fs/config/flags"
	"github.com/artpar/rclone/fs/filter/filterflags"
	"github.com/artpar/rclone/fs/log/logflags"
	"github.com/artpar/rclone/fs/rc/rcflags"
	"github.com/artpar/rclone/lib/atexit"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// Root is the main rclone command
var Root = &cobra.Command{
	Use:   "rclone",
	Short: "Show help for rclone commands, flags and backends.",
	Long: `
Rclone syncs files to and from cloud storage providers as well as
mounting them, listing them in lots of different ways.

See the home page (https://rclone.org/) for installation, usage,
documentation, changelog and configuration walkthroughs.

`,
	PersistentPostRun: func(cmd *cobra.Command, args []string) {
		fs.Debugf("rclone", "Version %q finishing with parameters %q", fs.Version, os.Args)
		atexit.Run()
	},
	BashCompletionFunction: bashCompletionFunc,
	DisableAutoGenTag:      true,
}

const (
	bashCompletionFunc = `
__rclone_custom_func() {
    if [[ ${#COMPREPLY[@]} -eq 0 ]]; then
        local cur cword prev words
        if declare -F _init_completion > /dev/null; then
            _init_completion -n : || return
        else
            __rclone_init_completion -n : || return
        fi
	local rclone=(command rclone --ask-password=false)
        if [[ $cur != *:* ]]; then
            local ifs=$IFS
            IFS=$'\n'
            local remotes=($("${rclone[@]}" listremotes 2> /dev/null))
            IFS=$ifs
            local remote
            for remote in "${remotes[@]}"; do
                [[ $remote != $cur* ]] || COMPREPLY+=("$remote")
            done
            if [[ ${COMPREPLY[@]} ]]; then
                local paths=("$cur"*)
                [[ ! -f ${paths[0]} ]] || COMPREPLY+=("${paths[@]}")
            fi
        else
            local path=${cur#*:}
            if [[ $path == */* ]]; then
                local prefix=$(eval printf '%s' "${path%/*}")
            else
                local prefix=
            fi
            local ifs=$IFS
            IFS=$'\n'
            local lines=($("${rclone[@]}" lsf "${cur%%:*}:$prefix" 2> /dev/null))
            IFS=$ifs
            local line
            for line in "${lines[@]}"; do
                local reply=${prefix:+$prefix/}$line
                [[ $reply != $path* ]] || COMPREPLY+=("$reply")
            done
	    [[ ! ${COMPREPLY[@]} || $(type -t compopt) != builtin ]] || compopt -o filenames
        fi
        [[ ! ${COMPREPLY[@]} || $(type -t compopt) != builtin ]] || compopt -o nospace
    fi
}
`
)

// GeneratingDocs is set by rclone gendocs to alter the format of the
// output suitable for the documentation.
var GeneratingDocs = false

// root help command
var helpCommand = &cobra.Command{
	Use:   "help",
	Short: Root.Short,
	Long:  Root.Long,
	Run: func(command *cobra.Command, args []string) {
		Root.SetOutput(os.Stdout)
		_ = Root.Usage()
	},
}

// to filter the flags with
var flagsRe *regexp.Regexp

// Show the flags
var helpFlags = &cobra.Command{
	Use:   "flags [<regexp to match>]",
	Short: "Show the global flags for rclone",
	Run: func(command *cobra.Command, args []string) {
		if len(args) > 0 {
			re, err := regexp.Compile(`(?i)` + args[0])
			if err != nil {
				log.Fatalf("Failed to compile flags regexp: %v", err)
			}
			flagsRe = re
		}
		if GeneratingDocs {
			Root.SetUsageTemplate(docFlagsTemplate)
		} else {
			Root.SetOutput(os.Stdout)
		}
		_ = command.Usage()
	},
}

// Show the backends
var helpBackends = &cobra.Command{
	Use:   "backends",
	Short: "List the backends available",
	Run: func(command *cobra.Command, args []string) {
		showBackends()
	},
}

// Show a single backend
var helpBackend = &cobra.Command{
	Use:   "backend <name>",
	Short: "List full info about a backend",
	Run: func(command *cobra.Command, args []string) {
		if len(args) == 0 {
			Root.SetOutput(os.Stdout)
			_ = command.Usage()
			return
		}
		showBackend(args[0])
	},
}

// runRoot implements the main rclone command with no subcommands
func runRoot(cmd *cobra.Command, args []string) {
	if version {
		ShowVersion()
		resolveExitCode(nil)
	} else {
		_ = cmd.Usage()
		if len(args) > 0 {
			_, _ = fmt.Fprintf(os.Stderr, "Command not found.\n")
		}
		resolveExitCode(errorCommandNotFound)
	}
}

// setupRootCommand sets default usage, help, and error handling for
// the root command.
//
// Helpful example: https://github.com/moby/moby/blob/master/cli/cobra.go
func setupRootCommand(rootCmd *cobra.Command) {
	ci := fs.GetConfig(context.Background())
	// Add global flags
	configflags.AddFlags(ci, pflag.CommandLine)
	filterflags.AddFlags(pflag.CommandLine)
	rcflags.AddFlags(pflag.CommandLine)
	logflags.AddFlags(pflag.CommandLine)

	Root.Run = runRoot
	Root.Flags().BoolVarP(&version, "version", "V", false, "Print the version number")

	cobra.AddTemplateFunc("showGlobalFlags", func(cmd *cobra.Command) bool {
		return cmd.CalledAs() == "flags" || cmd.Annotations["groups"] != ""
	})
	cobra.AddTemplateFunc("showCommands", func(cmd *cobra.Command) bool {
		return cmd.CalledAs() != "flags"
	})
	cobra.AddTemplateFunc("showLocalFlags", func(cmd *cobra.Command) bool {
		// Don't show local flags (which are the global ones on the root) on "rclone" and
		// "rclone help" (which shows the global help)
		return cmd.CalledAs() != "rclone" && cmd.CalledAs() != ""
	})
	cobra.AddTemplateFunc("flagGroups", func(cmd *cobra.Command) []*flags.Group {
		// Add the backend flags and check all flags
		backendGroup := flags.All.NewGroup("Backend", "Backend only flags. These can be set in the config file also.")
		allRegistered := flags.All.AllRegistered()
		cmd.InheritedFlags().VisitAll(func(flag *pflag.Flag) {
			if _, ok := backendFlags[flag.Name]; ok {
				backendGroup.Add(flag)
			} else if _, ok := allRegistered[flag]; ok {
				// flag is in a group already
			} else {
				fs.Errorf(nil, "Flag --%s is unknown", flag.Name)
			}
		})
		groups := flags.All.Filter(flagsRe).Include(cmd.Annotations["groups"])
		return groups.Groups
	})
	rootCmd.SetUsageTemplate(usageTemplate)
	// rootCmd.SetHelpTemplate(helpTemplate)
	// rootCmd.SetFlagErrorFunc(FlagErrorFunc)
	rootCmd.SetHelpCommand(helpCommand)
	// rootCmd.PersistentFlags().BoolP("help", "h", false, "Print usage")
	// rootCmd.PersistentFlags().MarkShorthandDeprecated("help", "please use --help")

	rootCmd.AddCommand(helpCommand)
	helpCommand.AddCommand(helpFlags)
	helpCommand.AddCommand(helpBackends)
	helpCommand.AddCommand(helpBackend)

	cobra.OnInitialize(initConfig)

}

var usageTemplate = `Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if and (showCommands .) .HasAvailableSubCommands}}

Available Commands:{{range .Commands}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if and (showLocalFlags .) .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if and (showGlobalFlags .) .HasAvailableInheritedFlags}}

{{ range flagGroups . }}{{ if .Flags.HasFlags }}
# {{ .Name }} Flags

{{ .Help }}

{{ .Flags.FlagUsages | trimTrailingWhitespaces}}
{{ end }}{{ end }}

Additional help topics:{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}

Use "rclone [command] --help" for more information about a command.
Use "rclone help flags" for to see the global flags.
Use "rclone help backends" for a list of supported services.
`

var docFlagsTemplate = `---
title: "Global Flags"
description: "Rclone Global Flags"
---

# Global Flags

This describes the global flags available to every rclone command
split into groups.

{{ range flagGroups . }}{{ if .Flags.HasFlags }}
## {{ .Name }}

{{ .Help }}

` + "```" + `
{{ .Flags.FlagUsages | trimTrailingWhitespaces}}
` + "```" + `

{{ end }}{{ end }}
`

// show all the backends
func showBackends() {
	fmt.Printf("All rclone backends:\n\n")
	for _, backend := range fs.Registry {
		fmt.Printf("  %-12s %s\n", backend.Prefix, backend.Description)
	}
	fmt.Printf("\nTo see more info about a particular backend use:\n")
	fmt.Printf("  rclone help backend <name>\n")
}

func quoteString(v interface{}) string {
	switch v.(type) {
	case string:
		return fmt.Sprintf("%q", v)
	}
	return fmt.Sprint(v)
}

// show a single backend
func showBackend(name string) {
	backend, err := fs.Find(name)
	if err != nil {
		log.Println(err)
	}
	var standardOptions, advancedOptions fs.Options
	done := map[string]struct{}{}
	for _, opt := range backend.Options {
		// Skip if done already (e.g. with Provider options)
		if _, doneAlready := done[opt.Name]; doneAlready {
			continue
		}
		done[opt.Name] = struct{}{}
		if opt.Advanced {
			advancedOptions = append(advancedOptions, opt)
		} else {
			standardOptions = append(standardOptions, opt)
		}
	}
	optionsType := "standard"
	for _, opts := range []fs.Options{standardOptions, advancedOptions} {
		if len(opts) == 0 {
			optionsType = "advanced"
			continue
		}
		optionsType = cases.Title(language.Und, cases.NoLower).String(optionsType)
		fmt.Printf("### %s options\n\n", optionsType)
		fmt.Printf("Here are the %s options specific to %s (%s).\n\n", optionsType, backend.Name, backend.Description)
		optionsType = "advanced"
		for _, opt := range opts {
			done[opt.Name] = struct{}{}
			shortOpt := ""
			if opt.ShortOpt != "" {
				shortOpt = fmt.Sprintf(" / -%s", opt.ShortOpt)
			}
			fmt.Printf("#### --%s%s\n\n", opt.FlagName(backend.Prefix), shortOpt)
			fmt.Printf("%s\n\n", opt.Help)
			if opt.IsPassword {
				fmt.Printf("**NB** Input to this must be obscured - see [rclone obscure](/commands/rclone_obscure/).\n\n")
			}
			fmt.Printf("Properties:\n\n")
			fmt.Printf("- Config:      %s\n", opt.Name)
			fmt.Printf("- Env Var:     %s\n", opt.EnvVarName(backend.Prefix))
			if opt.Provider != "" {
				fmt.Printf("- Provider:    %s\n", opt.Provider)
			}
			fmt.Printf("- Type:        %s\n", opt.Type())
			defaultValue := opt.GetValue()
			// Default value and Required are related: Required means option must
			// have a value, but if there is a default then a value does not have
			// to be explicitly set and then Required makes no difference.
			if defaultValue != "" {
				fmt.Printf("- Default:     %s\n", quoteString(defaultValue))
			} else {
				fmt.Printf("- Required:    %v\n", opt.Required)
			}
			// List examples / possible choices
			if len(opt.Examples) > 0 {
				if opt.Exclusive {
					fmt.Printf("- Choices:\n")
				} else {
					fmt.Printf("- Examples:\n")
				}
				for _, ex := range opt.Examples {
					fmt.Printf("    - %s\n", quoteString(ex.Value))
					for _, line := range strings.Split(ex.Help, "\n") {
						fmt.Printf("        - %s\n", line)
					}
				}
			}
			fmt.Printf("\n")
		}
	}
	if backend.MetadataInfo != nil {
		fmt.Printf("### Metadata\n\n")
		fmt.Printf("%s\n\n", strings.TrimSpace(backend.MetadataInfo.Help))
		if len(backend.MetadataInfo.System) > 0 {
			fmt.Printf("Here are the possible system metadata items for the %s backend.\n\n", backend.Name)
			keys := []string{}
			for k := range backend.MetadataInfo.System {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			fmt.Printf("| Name | Help | Type | Example | Read Only |\n")
			fmt.Printf("|------|------|------|---------|-----------|\n")
			for _, k := range keys {
				v := backend.MetadataInfo.System[k]
				ro := "N"
				if v.ReadOnly {
					ro = "**Y**"
				}
				fmt.Printf("| %s | %s | %s | %s | %s |\n", k, v.Help, v.Type, v.Example, ro)
			}
			fmt.Printf("\n")
		}
		fmt.Printf("See the [metadata](/docs/#metadata) docs for more info.\n\n")
	}
}
