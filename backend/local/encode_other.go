//+build !windows,!darwin

package local

import (
	"github.com/artpar/rclone/fs/encodings"
)

const enc = encodings.LocalUnix
