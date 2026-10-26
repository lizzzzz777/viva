package main

import (
	"bytes"
	"encoding/base64"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var windowsSigningVariables = []string{
	"WINDOWS_CERTIFICATE_FILE", "WINDOWS_CERTIFICATE", "WINDOWS_CERTIFICATE_PASSWORD", "WINDOWS_TIMESTAMP_URL", "WINDOWS_SIGN_COMMAND",
}

func TestSplitCommandGroupsQuotedWordsAndRefusesAnOpenQuote(t *testing.T) {
	words, err := splitCommand(`azuresign --file {file} --description "My App" --x="a b"`)
	if err != nil || strings.Join(words, "|") != `azuresign|--file|{file}|--description|My App|--x=a b` {
		t.Fatalf("%q, %v", words, err)
	}
	if _, err := splitCommand(`sign "{file}`); err == nil {
		t.Fatal("an open quote was accepted")
	}
}

func TestWindowsSigningNeedsOneWayAndTheFileToSign(t *testing.T) {
	none, err := resolveWindowsSigning(signingOptions{}, environment(nil))
	if err != nil || none.signs() {
		t.Fatalf("nothing set: %+v, %v", none, err)
	}
	if got, _ := resolveWindowsSigning(signingOptions{skip: true}, environment(map[string]string{"WINDOWS_SIGN_COMMAND": "x {file}"})); got.signs() {
		t.Fatal("--no-sign still signs")
	}
	pfx := filepath.Join(t.TempDir(), "c.pfx")
	if err := os.WriteFile(pfx, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		options signingOptions
		env     map[string]string
		want    string
	}{
		"command without {file}": {signingOptions{signCommand: "sign it"}, nil, "{file}"},
		"command and file":       {signingOptions{signCommand: "sign {file}"}, map[string]string{"WINDOWS_CERTIFICATE_FILE": pfx}, "use one or the other"},
		"file and base64":        {signingOptions{}, map[string]string{"WINDOWS_CERTIFICATE_FILE": pfx, "WINDOWS_CERTIFICATE": "eA=="}, "both set"},
		"missing file":           {signingOptions{}, map[string]string{"WINDOWS_CERTIFICATE_FILE": pfx + ".nope"}, "is not a file"},
		"bad base64":             {signingOptions{}, map[string]string{"WINDOWS_CERTIFICATE": "***"}, "not base64"},
	} {
		if _, err := resolveWindowsSigning(test.options, environment(test.env)); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error = %v, want it to contain %q", name, err, test.want)
		}
	}
	got, err := resolveWindowsSigning(signingOptions{}, environment(map[string]string{"WINDOWS_CERTIFICATE": base64.StdEncoding.EncodeToString([]byte("pfx bytes")), "WINDOWS_CERTIFICATE_PASSWORD": "pw"}))
	if err != nil || !got.signs() || got.timestamp != defaultTimestampURL || got.password != "pw" {
		t.Fatalf("%+v, %v", got, err)
	}
	if data, err := os.ReadFile(got.certificate); err != nil || string(data) != "pfx bytes" {
		t.Fatalf("scratch certificate: %q, %v", data, err)
	}
	if info, _ := os.Stat(got.certificate); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("the scratch certificate is readable by others: %v", info.Mode())
	}
	got.cleanup()
	if _, err := os.Stat(got.certificate); !os.IsNotExist(err) {
		t.Error("the scratch certificate was left behind")
	}
}

// signerStandIn answers like osslsigncode: it writes the signed copy where -out says.
func signerStandIn(r *recorder) toolRunner {
	return func(name string, arguments ...string) (string, error) {
		output, err := r.run(name, arguments...)
		if name == "osslsigncode" {
			var in, out string
			for index := 0; index+1 < len(arguments); index++ {
				switch arguments[index] {
				case "-in":
					in = arguments[index+1]
				case "-out":
					out = arguments[index+1]
				}
			}
			data, readErr := os.ReadFile(in)
			if readErr != nil {
				return "", readErr
			}
			if writeErr := os.WriteFile(out, append(data, []byte("<signed>")...), 0o644); writeErr != nil {
				return "", writeErr
			}
		}
		return output, err
	}
}

func TestWindowsPackagesAreSignedProgramFirstThenInstaller(t *testing.T) {
	fakeToolchain(t, fakeMakensis)
	r := &recorder{}
	previous := runSigningTool
	runSigningTool = signerStandIn(r)
	t.Cleanup(func() { runSigningTool = previous })
	pfx := filepath.Join(t.TempDir(), "c.pfx")
	if err := os.WriteFile(pfx, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := fixture(t, windowsTOML)
	t.Setenv("WINDOWS_CERTIFICATE_FILE", pfx)
	t.Setenv("WINDOWS_CERTIFICATE_PASSWORD", "hunter2")
	writeIcon(t, filepath.Join(dir, "icon.png"), 300)
	var output bytes.Buffer
	if err := runCLI([]string{"package", "-C", dir, "--target", "windows", "--arch", "amd64", "--format", "nsis,zip"}, &output, io.Discard); err != nil {
		t.Fatalf("%v\n%s", err, output.String())
	}
	if len(r.commands) != 2 || !strings.Contains(r.commands[0], "-in ") || !strings.Contains(r.commands[0], "fixture-notes") ||
		strings.Contains(r.commands[0], "-setup.exe") || !strings.Contains(r.commands[1], "-setup.exe") {
		t.Fatalf("commands:\n%s", strings.Join(r.commands, "\n"))
	}
	for _, command := range r.commands {
		if strings.Contains(command, "hunter2") || !strings.Contains(command, "-readpass ") || !strings.Contains(command, "-ts "+defaultTimestampURL) {
			t.Errorf("command %q passes the password on the command line or lacks the timestamp", command)
		}
	}
	// The zip holds the signed program; the installer was signed after it was made.
	files := readZip(t, filepath.Join(dir, "build", "fixture-notes-2.0.1-windows-amd64.zip"))
	if !bytes.HasSuffix(files["fixture-notes.exe"], []byte("<signed>")) {
		t.Error("the zip holds an unsigned program")
	}
	installer, _ := os.ReadFile(filepath.Join(dir, "build", "fixture-notes-2.0.1-windows-amd64-setup.exe"))
	if !bytes.HasSuffix(installer, []byte("<signed>")) {
		t.Errorf("the installer is unsigned: %q", installer)
	}
	if !strings.Contains(output.String(), "signing  signed fixture-notes.exe, fixture-notes-2.0.1-windows-amd64-setup.exe with the certificate") {
		t.Errorf("the report:\n%s", output.String())
	}
}

func TestACustomSignCommandRunsOnEveryFileAndASuppliedProgramIsLeftAlone(t *testing.T) {
	r := &recorder{}
	previous := runSigningTool
	runSigningTool = r.run
	t.Cleanup(func() { runSigningTool = previous })
	dir := fixture(t, windowsTOML)
	var output bytes.Buffer
	if err := runCLI([]string{"package", "-C", dir, "--target", "windows", "--arch", "amd64", "--format", "zip", "--sign-command", `cloudsign --file "{file}" --name "Fixture Notes"`}, &output, io.Discard); err != nil {
		t.Fatalf("%v\n%s", err, output.String())
	}
	if len(r.commands) != 1 || !strings.HasPrefix(r.commands[0], "cloudsign --file ") || !strings.HasSuffix(r.commands[0], " --name Fixture Notes") {
		t.Fatalf("commands: %q", r.commands)
	}

	// An executable supplied with --program was signed (or not) by its owner.
	program := filepath.Join(t.TempDir(), "mine.exe")
	if err := os.WriteFile(program, []byte("MZ mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.commands = nil
	output.Reset()
	if err := runCLI([]string{"package", "-C", dir, "--target", "windows", "--arch", "amd64", "--format", "zip", "--program", program, "--sign-command", "cloudsign {file}"}, &output, io.Discard); err != nil {
		t.Fatalf("%v\n%s", err, output.String())
	}
	if len(r.commands) != 0 || !strings.Contains(output.String(), "supplied with --program") {
		t.Fatalf("commands %q\n%s", r.commands, output.String())
	}
}

func TestACommandThatFailsFailsThePackageWithItsOutput(t *testing.T) {
	r := &recorder{failOn: "cloudsign"}
	previous := runSigningTool
	runSigningTool = r.run
	t.Cleanup(func() { runSigningTool = previous })
	dir := fixture(t, windowsTOML)
	err := runCLI([]string{"package", "-C", dir, "--target", "windows", "--arch", "amd64", "--format", "zip", "--sign-command", "cloudsign {file}"}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("a failed signing step passed")
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "build")); len(entries) != 0 {
		t.Errorf("a failed run left %d files", len(entries))
	}
}

func TestOpenSSLSignsARealProgramAndTheSignatureVerifies(t *testing.T) {
	if _, err := exec.LookPath("osslsigncode"); err != nil {
		t.Skip("osslsigncode is not installed")
	}
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("no openssl to make a certificate")
	}
	scratch := t.TempDir()
	run := func(name string, arguments ...string) {
		t.Helper()
		if out, err := exec.Command(name, arguments...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, arguments, err, out)
		}
	}
	run("openssl", "req", "-x509", "-newkey", "rsa:2048", "-keyout", filepath.Join(scratch, "k.pem"), "-out", filepath.Join(scratch, "c.pem"),
		"-days", "2", "-nodes", "-subj", "/CN=Viva Test Signing", "-addext", "keyUsage=critical,digitalSignature", "-addext", "extendedKeyUsage=critical,codeSigning")
	run("openssl", "pkcs12", "-export", "-inkey", filepath.Join(scratch, "k.pem"), "-in", filepath.Join(scratch, "c.pem"), "-out", filepath.Join(scratch, "c.pfx"), "-passout", "pass:pw")
	pfx, err := os.ReadFile(filepath.Join(scratch, "c.pfx"))
	if err != nil {
		t.Fatal(err)
	}
	signing, err := resolveWindowsSigning(signingOptions{}, environment(map[string]string{
		"WINDOWS_CERTIFICATE": base64.StdEncoding.EncodeToString(pfx), "WINDOWS_CERTIFICATE_PASSWORD": "pw",
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer signing.cleanup()
	signing.timestamp = "" // no timestamp server is reached from a test; the signing is real

	program := filepath.Join(scratch, "program.exe")
	dir := fixture(t, windowsTOML)
	build := exec.Command("go", "build", "-trimpath", "-o", program, ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building a program: %v\n%s", err, out)
	}
	before, _ := os.ReadFile(program)
	if err := signing.signFile(execTool, program); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(program)
	if len(after) <= len(before) {
		t.Fatal("the program was not changed by signing")
	}
	if out, err := exec.Command("osslsigncode", "verify", "-CAfile", filepath.Join(scratch, "c.pem"), "-in", program).CombinedOutput(); err != nil || !strings.Contains(string(out), "Signature verification: ok") {
		t.Fatalf("verify: %v\n%s", err, out)
	}
	if entries, _ := filepath.Glob(program + "*"); len(entries) != 1 {
		t.Errorf("signing left files beside the program: %v", entries)
	}
}
