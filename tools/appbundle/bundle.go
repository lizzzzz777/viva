package appbundle

import (
	"archive/zip"
	"bytes"
	"fmt"
	"github.com/vivaui/viva/internal/iconshape"
	"image/png"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// IconFileName is the icon's file name inside Contents/Resources.
const IconFileName = "AppIcon.icns"

// Options describes one bundle to build.
type Options struct {
	// Dest is the .app directory to create.
	Dest string
	Info Info
	// Executable is the built program to copy into Contents/MacOS.
	Executable string
	// Icon is an optional .png (square, at least MinIconPixels) or .icns file.
	Icon string
}

// Result reports what was built.
type Result struct {
	Path            string
	ExecutableBytes int64
	// BundleBytes is the total size of the files in the bundle.
	BundleBytes int64
	// IconPixels is the largest icon representation, or zero without an icon.
	IconPixels int
}

// Assemble builds the bundle in a sibling directory and swaps it into place, so
// a failure leaves the previous bundle untouched. It refuses to replace
// anything that is not an application bundle.
func Assemble(options Options) (Result, error) {
	dest := filepath.Clean(options.Dest)
	if !strings.HasSuffix(dest, ".app") {
		return Result{}, fmt.Errorf("appbundle: %s is not a .app path", dest)
	}
	if info, err := os.Stat(dest); err == nil {
		if _, plistErr := os.Stat(filepath.Join(dest, "Contents", "Info.plist")); !info.IsDir() || plistErr != nil {
			return Result{}, fmt.Errorf("appbundle: %s exists and is not an application bundle; refusing to replace it", dest)
		}
	} else if !os.IsNotExist(err) {
		return Result{}, err
	}

	info := options.Info
	var icon []byte
	var iconPixels int
	if options.Icon != "" {
		var err error
		if icon, iconPixels, err = LoadIcon(options.Icon); err != nil {
			return Result{}, err
		}
		info.IconFile = IconFileName
	}
	plist, err := info.Plist()
	if err != nil {
		return Result{}, err
	}
	program, err := os.ReadFile(options.Executable)
	if err != nil {
		return Result{}, fmt.Errorf("appbundle: reading the program: %w", err)
	}

	staging := dest + ".viva-tmp"
	if err := os.RemoveAll(staging); err != nil {
		return Result{}, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			os.RemoveAll(staging)
		}
	}()
	contents := filepath.Join(staging, "Contents")
	if err := os.MkdirAll(filepath.Join(contents, "MacOS"), 0o755); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(filepath.Join(contents, "MacOS", info.Executable), program, 0o755); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(filepath.Join(contents, "Info.plist"), plist, 0o644); err != nil {
		return Result{}, err
	}
	if icon != nil {
		if err := os.MkdirAll(filepath.Join(contents, "Resources"), 0o755); err != nil {
			return Result{}, err
		}
		if err := os.WriteFile(filepath.Join(contents, "Resources", IconFileName), icon, 0o644); err != nil {
			return Result{}, err
		}
	}

	if err := os.RemoveAll(dest); err != nil {
		return Result{}, err
	}
	if err := os.Rename(staging, dest); err != nil {
		return Result{}, err
	}
	cleanup = false

	result := Result{Path: dest, ExecutableBytes: int64(len(program)), IconPixels: iconPixels}
	err = filepath.WalkDir(dest, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		info, err := entry.Info()
		if err == nil {
			result.BundleBytes += info.Size()
		}
		return err
	})
	return result, err
}

// LoadIcon reads an icon file as .icns data. A .png is converted; an .icns is
// used as it is after checking its header. pixels is the largest size inside.
func LoadIcon(path string) (data []byte, pixels int, err error) {
	source, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("appbundle: reading the icon: %w", err)
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".icns":
		if len(source) < 8 || string(source[:4]) != "icns" {
			return nil, 0, fmt.Errorf("appbundle: %s is not an .icns file", path)
		}
		return source, 0, nil
	case ".png":
		decoded, err := png.Decode(bytes.NewReader(source))
		if err != nil {
			return nil, 0, fmt.Errorf("appbundle: %s: %w", path, err)
		}
		// The picture is a full-bleed square unless it brings its own shape; the
		// tile and its margin are the platform's convention, not the artist's.
		data, pixels, err = EncodeICNS(iconshape.Tile(decoded, decoded.Bounds().Dx(), iconshape.MacInset))
		if err != nil {
			return nil, 0, fmt.Errorf("%w (%s)", err, path)
		}
		return data, pixels, nil
	}
	return nil, 0, fmt.Errorf("appbundle: the icon %s must be a .png or .icns file", path)
}

// zipTime is the fixed timestamp of every zip entry, so the same bundle always
// produces the same archive bytes.
var zipTime = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

// Zip archives the bundle with the .app directory as the top-level entry,
// keeping file modes (the program stays executable) and using fixed timestamps.
func Zip(app, archive string) error {
	plist, err := os.ReadFile(filepath.Join(app, "Contents", "Info.plist"))
	if err != nil {
		return fmt.Errorf("appbundle: %s is not an application bundle", app)
	}
	executable, err := plistExecutable(plist)
	if err != nil {
		return err
	}
	program := filepath.Join("Contents", "MacOS", executable)
	if info, err := os.Lstat(filepath.Join(app, program)); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("appbundle: executable %s is not a regular file", executable)
	}
	out, err := os.Create(archive)
	if err != nil {
		return err
	}
	writer := zip.NewWriter(out)
	root := filepath.Base(app)
	err = filepath.WalkDir(app, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(app, path)
		if err != nil {
			return err
		}
		name := root
		if relative != "." {
			name = root + "/" + filepath.ToSlash(relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		header := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: zipTime}
		if entry.IsDir() {
			header.Name += "/"
			header.Method = zip.Store
			header.SetMode(fs.ModeDir | 0o755)
			_, err := writer.CreateHeader(header)
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("appbundle: %s is not a regular file", path)
		}
		mode := fs.FileMode(0o644)
		// Windows cannot carry Unix execute bits. The declared main program
		// must still be executable after the archive is unpacked on macOS.
		if relative == program || info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		header.SetMode(mode)
		file, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}
		source, err := os.Open(path)
		if err != nil {
			return err
		}
		defer source.Close()
		_, err = io.Copy(file, source)
		return err
	})
	if err == nil {
		err = writer.Close()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(archive)
	}
	return err
}
