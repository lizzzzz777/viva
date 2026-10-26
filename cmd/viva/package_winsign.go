package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Authenticode signing of a Windows package is optional, like the macOS signing
// in package_sign.go: with nothing configured nothing runs. Two ways, both read
// from the environment so no secret reaches viva.toml or a process list the shell
// history keeps:
//
//	WINDOWS_CERTIFICATE_FILE      a .pfx to sign with, or
//	WINDOWS_CERTIFICATE           the same, base64 (written to a private scratch file)
//	WINDOWS_CERTIFICATE_PASSWORD  its password
//	WINDOWS_TIMESTAMP_URL         an RFC 3161 timestamp server (default below)
//
// signed with osslsigncode, which runs on any host, or with Microsoft's signtool
// on Windows when osslsigncode is absent; or
//
//	WINDOWS_SIGN_COMMAND / --sign-command   a command that signs {file} in place
//
// for certificates whose keys stay in a hardware token or a cloud signing
// service (Azure Trusted Signing, a company HSM), which no file can stand in for.
// The command is split into words at blanks (double quotes group) and run
// without a shell; {file} is replaced inside a word by the path to sign.

const defaultTimestampURL = "http://timestamp.digicert.com"

// windowsSigning is how a Windows package run signs, or the zero value.
type windowsSigning struct {
	certificate string // path to a .pfx
	password    string
	timestamp   string
	command     []string // words of a custom sign command, with {file} in them
	cleanup     func()
}

func (signing windowsSigning) signs() bool {
	return signing.certificate != "" || signing.command != nil
}

// splitCommand splits a command line into words; double quotes group a word.
func splitCommand(line string) ([]string, error) {
	var words []string
	var current strings.Builder
	inWord, quoted := false, false
	for _, r := range line {
		switch {
		case r == '"':
			quoted, inWord = !quoted, true
		case (r == ' ' || r == '\t') && !quoted:
			if inWord {
				words = append(words, current.String())
				current.Reset()
				inWord = false
			}
		default:
			current.WriteRune(r)
			inWord = true
		}
	}
	if quoted {
		return nil, errors.New("the sign command has an unclosed double quote")
	}
	if inWord {
		words = append(words, current.String())
	}
	return words, nil
}

// resolveWindowsSigning decides from the options and the environment how to
// sign. A certificate given as base64 is written to a scratch file that cleanup
// removes.
func resolveWindowsSigning(options signingOptions, environment func(string) string) (windowsSigning, error) {
	signing := windowsSigning{cleanup: func() {}}
	if options.skip {
		return signing, nil
	}
	line := options.signCommand
	if line == "" {
		line = environment("WINDOWS_SIGN_COMMAND")
	}
	file, encoded := environment("WINDOWS_CERTIFICATE_FILE"), environment("WINDOWS_CERTIFICATE")
	switch {
	case line != "":
		if file != "" || encoded != "" {
			return signing, errors.New("a sign command and a certificate were both given; the command is for keys that are not in a file, so use one or the other")
		}
		words, err := splitCommand(line)
		if err != nil {
			return signing, err
		}
		if len(words) == 0 || !strings.Contains(line, "{file}") {
			return signing, errors.New("the sign command must name the file it signs as {file}")
		}
		signing.command = words
	case file != "" && encoded != "":
		return signing, errors.New("WINDOWS_CERTIFICATE_FILE and WINDOWS_CERTIFICATE are both set; use one")
	case file != "":
		abs, err := filepath.Abs(file)
		if err != nil {
			return signing, err
		}
		if info, statErr := os.Stat(abs); statErr != nil || !info.Mode().IsRegular() {
			return signing, fmt.Errorf("WINDOWS_CERTIFICATE_FILE %s is not a file", displayPath(abs))
		}
		signing.certificate = abs
	case encoded != "":
		data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
		if err != nil {
			return signing, errors.New("WINDOWS_CERTIFICATE is not base64")
		}
		dir, err := signingTempDir()
		if err != nil {
			return signing, err
		}
		signing.cleanup = func() { _ = os.RemoveAll(dir) }
		signing.certificate = filepath.Join(dir, "certificate.pfx")
		if err := os.WriteFile(signing.certificate, data, 0o600); err != nil {
			signing.cleanup()
			return signing, err
		}
	default:
		return signing, nil
	}
	signing.password = environment("WINDOWS_CERTIFICATE_PASSWORD")
	signing.timestamp = environment("WINDOWS_TIMESTAMP_URL")
	if signing.timestamp == "" {
		signing.timestamp = defaultTimestampURL
	}
	return signing, nil
}

// signFile signs an executable or an installer in place.
func (signing windowsSigning) signFile(run toolRunner, path string) error {
	if signing.command != nil {
		words := make([]string, len(signing.command))
		for index, word := range signing.command {
			words[index] = strings.ReplaceAll(word, "{file}", path)
		}
		_, err := run(words[0], words[1:]...)
		return err
	}
	if _, err := exec.LookPath("osslsigncode"); err == nil || runtime.GOOS != "windows" {
		return signing.signWithOpenSSL(run, path)
	}
	// Microsoft's tool takes the password on its command line; there is no other way.
	arguments := []string{"sign", "/fd", "sha256", "/tr", signing.timestamp, "/td", "sha256", "/f", signing.certificate}
	if signing.password != "" {
		arguments = append(arguments, "/p", signing.password)
	}
	_, err := run("signtool", append(arguments, path)...)
	return err
}

// signWithOpenSSL signs with osslsigncode, which writes a new file rather than
// changing the one it reads, then replaces the original. The password goes in a
// private file, not on the command line.
func (signing windowsSigning) signWithOpenSSL(run toolRunner, path string) error {
	scratch, err := signingTempDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	signed := filepath.Join(scratch, filepath.Base(path))
	arguments := []string{"sign", "-pkcs12", signing.certificate, "-h", "sha256"}
	if signing.timestamp != "" {
		arguments = append(arguments, "-ts", signing.timestamp)
	}
	if signing.password != "" {
		passwordFile := filepath.Join(scratch, "password")
		if err := os.WriteFile(passwordFile, []byte(signing.password), 0o600); err != nil {
			return err
		}
		arguments = append(arguments, "-readpass", passwordFile)
	}
	arguments = append(arguments, "-in", path, "-out", signed)
	if _, err := run("osslsigncode", arguments...); err != nil {
		return err
	}
	return replaceFile(signed, path)
}

// replaceFile moves from over to, keeping to's permissions.
func replaceFile(from, to string) error {
	info, err := os.Stat(to)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	temporary := to + ".viva-tmp"
	if err := os.WriteFile(temporary, data, info.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Rename(temporary, to); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func reportWindowsSigning(output io.Writer, signing windowsSigning, files ...string) {
	names := make([]string, len(files))
	for index, file := range files {
		names[index] = filepath.Base(file)
	}
	how := "with the certificate"
	if signing.command != nil {
		how = "with the sign command"
	}
	reportSigning(output, "signing", fmt.Sprintf("signed %s %s", strings.Join(names, ", "), how))
}
