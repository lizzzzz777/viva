package appbundle

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func TestZipRestoresTheDeclaredExecutableWithoutMakingDataExecutable(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "Example.app")
	if _, err := Assemble(Options{Dest: app, Info: sampleInfo(), Executable: fakeProgram(t, dir)}); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(app, "Contents", "MacOS", sampleInfo().Executable)
	if err := os.Chmod(program, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "Contents", "MacOS", "data.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "app.zip")
	if err := Zip(app, archive); err != nil {
		t.Fatal(err)
	}
	reader, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, entry := range reader.File {
		if entry.FileInfo().IsDir() {
			continue
		}
		want := os.FileMode(0o644)
		if entry.Name == "Example.app/Contents/MacOS/"+sampleInfo().Executable {
			want = 0o755
		}
		if entry.Mode().Perm() != want {
			t.Errorf("%s: mode=%o want=%o", entry.Name, entry.Mode().Perm(), want)
		}
	}
}

func TestPlistExecutableIsATopLevelFilename(t *testing.T) {
	for _, body := range []string{
		`<key>Nested</key><dict><key>CFBundleExecutable</key><string>hidden</string></dict>`,
		`<key>CFBundleExecutable</key><integer>1</integer>`,
		`<key>CFBundleExecutable</key><string>../escape</string>`,
		`<key>CFBundleExecutable</key><string>sub\escape</string>`,
		`<key>CFBundleExecutable</key><string>C:escape</string>`,
		`<key>CFBundleExecutable</key><string></string>`,
		`<key>CFBundleExecutable</key><string>one</string><key>CFBundleExecutable</key><string>two</string>`,
		`<key>CFBundleExecutable</key><string>one</string><dict>`,
	} {
		if name, err := plistExecutable([]byte(`<plist><dict>` + body + `</dict></plist>`)); err == nil {
			t.Errorf("accepted %q: %q", body, name)
		}
	}
	name, err := plistExecutable([]byte(`<plist><dict><key>Nested</key><dict><key>CFBundleExecutable</key><string>hidden</string></dict><key>CFBundleExecutable</key><string>主机 &amp; shell</string></dict></plist>`))
	if err != nil || name != "主机 & shell" {
		t.Fatalf("name=%q err=%v", name, err)
	}
}
