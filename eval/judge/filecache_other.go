//go:build !unix

package judge

import "io/fs"

// checkPrivateDir cannot read ownership here, so modes that serve stored
// verdicts refuse every cache directory.
func checkPrivateDir(fs.FileInfo) error { return errOwnerUnchecked }
