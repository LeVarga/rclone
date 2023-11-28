package archive

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/cache"
	"github.com/rclone/rclone/fs/filter"
	"github.com/rclone/rclone/fs/operations"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FIXME need to test Open with seek
// FIXME need to test NewObject

// run - run a shell command
func run(t *testing.T, args ...string) {
	cmd := exec.Command(args[0], args[1:]...)
	fs.Debugf(nil, "run args = %v", args)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf(`
----------------------------
Failed to run %v: %v
Command output was:
%s
----------------------------
`, args, err, out)
	}
}

// check the dst and src are identical
func checkTree(ctx context.Context, name string, t *testing.T, dst, src string, expectedCount int) {
	t.Run(name, func(t *testing.T) {
		fs.Debugf(nil, "check %q vs %q", dst, src)
		Fdst, err := cache.Get(ctx, src)
		if err != fs.ErrorIsFile {
			require.NoError(t, err)
		}
		Fsrc, err := cache.Get(ctx, dst)
		if err != fs.ErrorIsFile {
			require.NoError(t, err)
		}

		var matches bytes.Buffer
		opt := operations.CheckOpt{
			Fdst:  Fdst,
			Fsrc:  Fsrc,
			Match: &matches,
		}

		for _, action := range []string{"Check", "Download"} {
			t.Run(action, func(t *testing.T) {
				matches.Reset()
				if action == "Download" {
					assert.NoError(t, operations.CheckDownload(ctx, &opt))
				} else {
					assert.NoError(t, operations.Check(ctx, &opt))
				}
				if expectedCount > 0 {
					assert.Equal(t, expectedCount, strings.Count(matches.String(), "\n"))
				}
			})
		}

		// t.Logf("Fdst ------------- %v", Fdst)
		// operations.List(ctx, Fdst, os.Stdout)
		// t.Logf("Fsrc ------------- %v", Fsrc)
		// operations.List(ctx, Fsrc, os.Stdout)
	})

}

// test creating and reading back some archives
//
// Note that this uses rclone and zip as external binaries.
func testArchive(t *testing.T, archiveName string, archiveFn func(t *testing.T, output, input string)) {
	ctx := context.Background()
	checkFiles := 1000

	// create random test input files
	inputRoot := t.TempDir()
	input := filepath.Join(inputRoot, archiveName)
	require.NoError(t, os.Mkdir(input, 0777))
	run(t, "rclone", "test", "makefiles", "--files", strconv.Itoa(checkFiles), input)

	// Create the archive
	output := t.TempDir()
	zipFile := path.Join(output, archiveName)
	archiveFn(t, zipFile, input)

	// Check the archive itself
	checkTree(ctx, "Archive", t, ":archive:"+zipFile, input, checkFiles)

	// Now check a subdirectory
	fis, err := os.ReadDir(input)
	require.NoError(t, err)
	subDir := "NOT FOUND"
	aFile := "NOT FOUND"
	for _, fi := range fis {
		if fi.IsDir() {
			subDir = fi.Name()
		} else {
			aFile = fi.Name()
		}
	}
	checkTree(ctx, "SubDir", t, ":archive:"+zipFile+"/"+subDir, filepath.Join(input, subDir), 0)

	// Now check a single file
	fiCtx, fi := filter.AddConfig(ctx)
	require.NoError(t, fi.AddRule("+ "+aFile))
	require.NoError(t, fi.AddRule("- *"))
	checkTree(fiCtx, "SingleFile", t, ":archive:"+zipFile+"/"+aFile, filepath.Join(input, aFile), 0)

	// Now check the level above
	checkTree(ctx, "Root", t, ":archive:"+output, inputRoot, checkFiles)
	// run(t, "cp", "-a", inputRoot, output, "/tmp/test-"+archiveName)
}

// Make sure we have the executable named
func skipIfNoExe(t *testing.T, exeName string) {
	_, err := exec.LookPath(exeName)
	if err != nil {
		t.Skipf("%s executable not installed", exeName)
	}
}

// Test creating and reading back some archives
//
// Note that this uses rclone and zip as external binaries.
func TestArchiveZip(t *testing.T) {
	skipIfNoExe(t, "zip")
	skipIfNoExe(t, "rclone")
	testArchive(t, "test.zip", func(t *testing.T, output, input string) {
		oldcwd, err := os.Getwd()
		require.NoError(t, err)
		require.NoError(t, os.Chdir(input))
		defer func() {
			require.NoError(t, os.Chdir(oldcwd))
		}()
		run(t, "zip", "-9r", output, ".")
	})
}

// Test creating and reading back some archives
//
// Note that this uses rclone and squashfs as external binaries.
func TestArchiveSquashfs(t *testing.T) {
	skipIfNoExe(t, "mksquashfs")
	skipIfNoExe(t, "rclone")
	testArchive(t, "test.sqfs", func(t *testing.T, output, input string) {
		run(t, "mksquashfs", input, output)
	})
}