package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode"
)

// AtomicWriteFile writes data to outfile by writing to a temporary file in the
// same directory and renaming it into place, so a reader never observes a
// partially written file. perm is applied to the resulting file.
func AtomicWriteFile(
	outfile string,
	data []byte,
	perm os.FileMode,
) (err error) {
	// TODO: provide better atomicity guarantees, like fsyncing the parent
	// directory and, on windows, using MoveFileEx with MOVEFILE_WRITE_THROUGH.

	fh, err := os.CreateTemp(filepath.Dir(outfile), filepath.Base(outfile))
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	needsClose, needsRemove := true, true

	defer func() {
		if needsClose {
			err = errors.Join(err, fh.Close())
		}
		if needsRemove {
			err = errors.Join(err, os.Remove(fh.Name()))
		}
	}()

	if _, err := fh.Write(data); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}

	if err := fh.Chmod(perm); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}

	needsClose = false
	if err := fh.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Rename(fh.Name(), outfile); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	needsRemove = false

	return nil
}

// ApplicationDir returns the best base directory for the current OS, joined
// with subdir. Each subdir element is capitalized on Windows and macOS and
// lowercased elsewhere, following each platform's convention.
func ApplicationDir(subdir ...string) string {
	subdir = slices.Clone(subdir)
	for i := range subdir {
		if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
			subdir[i] = capitalizeWords(subdir[i])
		} else {
			subdir[i] = strings.ToLower(subdir[i])
		}
	}
	var appdir string
	home := os.Getenv("HOME")

	switch runtime.GOOS {
	case "windows":
		// Windows standards: https://msdn.microsoft.com/en-us/library/windows/apps/hh465094.aspx
		for _, env := range []string{"AppData", "AppDataLocal", "UserProfile", "Home"} {
			val := os.Getenv(env)
			if val != "" {
				appdir = val
				break
			}
		}
	case "darwin":
		// Mac standards: https://developer.apple.com/library/archive/documentation/FileManagement/Conceptual/FileSystemProgrammingGuide/MacOSXDirectories/MacOSXDirectories.html
		appdir = filepath.Join(home, "Library", "Application Support")
	default:
		// Linux standards: https://specifications.freedesktop.org/basedir-spec/basedir-spec-latest.html
		appdir = os.Getenv("XDG_DATA_HOME")
		if appdir == "" && home != "" {
			appdir = filepath.Join(home, ".local", "share")
		}
	}
	return filepath.Join(append([]string{appdir}, subdir...)...)
}

// capitalizeWords upper-cases the first rune of each space-separated word,
// replacing the deprecated strings.Title for this narrow use.
func capitalizeWords(val string) string {
	words := strings.Split(val, " ")
	for i, word := range words {
		runes := []rune(word)
		if len(runes) == 0 {
			continue
		}
		runes[0] = unicode.ToUpper(runes[0])
		words[i] = string(runes)
	}
	return strings.Join(words, " ")
}

// IsValidSetupDir reports whether name is a valid directory to set up a
// configuration in: it must either not exist, or not already contain a
// config file.
func IsValidSetupDir(name string) (ok bool, err error) {
	_, err = os.Stat(name)
	if err != nil {
		if os.IsNotExist(err) {
			return true, err
		}
		return false, err
	}

	/* #nosec G304 */ // The function limits later on paths having a config file
	f, err := os.Open(name)
	if err != nil {
		return false, err
	}
	defer func() {
		err = errors.Join(err, f.Close())
	}()

	for {
		var filenames []string
		filenames, err = f.Readdirnames(100)
		if errors.Is(err, io.EOF) {
			// nothing more
			return true, nil
		} else if err != nil {
			// something went wrong
			return false, err
		}

		if slices.Contains(filenames, DefaultCfgFilename) {
			return false, nil
		}
	}
}

// IsWritable reports whether dir is a directory the current user can create
// files in.
func IsWritable(dir string) (bool, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return false, err
	}

	if !info.IsDir() {
		return false, fmt.Errorf("path %s is not a directory", dir)
	}

	// Check if the user bit is enabled in file permission
	if info.Mode().Perm()&0o200 == 0 {
		return false, errors.New(
			"write permission bit is not set on this file for user",
		)
	}

	// There is no OS cross-compatible method for determining if a user has
	// write permissions on a folder, so test by creating a file in it.
	testFile := filepath.Join(dir, ".perm")
	file, err := os.Create(testFile)
	if err != nil {
		return false, errors.New(
			"write permission bit is not set on this file for user",
		)
	}

	_ = file.Close()
	_ = os.Remove(testFile)

	return true, nil
}
