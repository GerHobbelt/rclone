package moveto

import (
	"context"

	"github.com/artpar/rclone/cmd"
	"github.com/artpar/rclone/fs/operations"
	"github.com/artpar/rclone/fs/sync"
	"github.com/spf13/cobra"
)

func init() {
	cmd.Root.AddCommand(commandDefinition)
}

var commandDefinition = &cobra.Command{
	Use:   "moveto source:path dest:path",
	Short: `Move file or directory from source to dest.`,
	Long: `
If source:path is a file or directory then it moves it to a file or
directory named dest:path.

This can be used to rename files or upload single files to other than
their existing name.  If the source is a directory then it acts exactly
like the move command.

So

    rclone moveto src dst

where src and dst are rclone paths, either remote:path or
/path/to/local or C:\windows\path\if\on\windows.

This will:

    if src is file
        move it to dst, overwriting an existing file if it exists
    if src is directory
        move it to dst, overwriting existing files if they exist
        see move command for full details

This doesn't transfer unchanged files, testing by size and
modification time or MD5SUM.  src will be deleted on successful
transfer.

**Important**: Since this can cause data loss, test first with the
--dry-run flag.

**Note**: Use the ` + "`-P`" + `/` + "`--progress`" + ` flag to view real-time transfer statistics.
`,
	Run: func(command *cobra.Command, args []string) {
		cmd.CheckArgs(2, 2, command, args)
		fsrc, srcFileName, fdst, dstFileName := cmd.NewFsSrcDstFiles(args)

		cmd.Run(true, true, command, func() error {
			if srcFileName == "" {
				return sync.MoveDir(context.Background(), fdst, fsrc, false, false)
			}
			return operations.MoveFile(context.Background(), fdst, fsrc, dstFileName, srcFileName)
		})
	},
}
