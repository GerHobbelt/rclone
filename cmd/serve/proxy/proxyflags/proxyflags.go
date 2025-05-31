// Package proxyflags implements command line flags to set up a proxy
package proxyflags

import (
	"github.com/artpar/rclone/cmd/serve/proxy"
	"github.com/artpar/rclone/fs/config/flags"
	"github.com/spf13/pflag"
)

// AddFlags adds the non filing system specific flags to the command
func AddFlags(flagSet *pflag.FlagSet) {
	flags.AddFlagsFromOptions(flagSet, "", proxy.OptionsInfo)
}
