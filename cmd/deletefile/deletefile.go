package deletefile

import (
	"github.com/artpar/rclone/cmd"
	"github.com/artpar/rclone/fs/operations"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

func init() {
	cmd.Root.AddCommand(commandDefintion)
}

var commandDefintion = &cobra.Command{
	Use:   "deletefile remote:path",
	Short: `Remove a single file path from remote.`,
	Long: `
Remove a single file path from remote.  Unlike ` + "`" + `delete` + "`" + ` it cannot be used to
remove a directory and it doesn't obey include/exclude filters - if the specified file exists,
it will always be removed.
`,
	Run: func(command *cobra.Command, args []string) {
		cmd.CheckArgs(1, 1, command, args)
		fs, fileName := cmd.NewFsFile(args[0])
		cmd.Run(true, false, command, func() error {
			if fileName == "" {
				return errors.Errorf("%s is a directory or doesn't exist", args[0])
			}
			fileObj, err := fs.NewObject(fileName)
			if err != nil {
				return err
			}
			return operations.DeleteFile(fileObj)
		})
	},
}
