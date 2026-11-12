package store

// The rclone backends built into the program. Each one adds to the size of the binary and to what has to be kept up to date: add one when it is asked for.
import (
	_ "github.com/rclone/rclone/backend/ftp"
	_ "github.com/rclone/rclone/backend/local"
	_ "github.com/rclone/rclone/backend/s3"
	_ "github.com/rclone/rclone/backend/sftp"
	_ "github.com/rclone/rclone/backend/webdav"
)
