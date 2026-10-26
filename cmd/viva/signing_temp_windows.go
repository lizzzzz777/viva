package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

var signingAdvapi = syscall.NewLazyDLL("advapi32.dll")
var signingSDFromString = signingAdvapi.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")

func signingUserSID() (string, error) {
	process, err := syscall.GetCurrentProcess()
	if err != nil {
		return "", err
	}
	var token syscall.Token
	if err := syscall.OpenProcessToken(process, syscall.TOKEN_QUERY, &token); err != nil {
		return "", err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String()
}

// Supply a protected DACL at creation, before writing any certificate/password.
// Children inherit access for this user and SYSTEM, never the parent temp ACL.
// https://learn.microsoft.com/windows/win32/fileio/file-security-and-access-rights
func signingTempDir() (string, error) {
	sid, err := signingUserSID()
	if err != nil {
		return "", fmt.Errorf("signing user: %w", err)
	}
	sddl, err := syscall.UTF16PtrFromString("D:P(A;OICI;FA;;;" + sid + ")(A;OICI;FA;;;SY)")
	if err != nil {
		return "", err
	}
	var descriptor uintptr
	if ok, _, err := signingSDFromString.Call(uintptr(unsafe.Pointer(sddl)), 1, uintptr(unsafe.Pointer(&descriptor)), 0); ok == 0 {
		return "", fmt.Errorf("signing directory security: %w", err)
	}
	defer syscall.LocalFree(syscall.Handle(descriptor))
	attributes := syscall.SecurityAttributes{Length: uint32(unsafe.Sizeof(syscall.SecurityAttributes{})), SecurityDescriptor: descriptor}
	for attempt := 0; attempt < 10; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", err
		}
		path := filepath.Join(os.TempDir(), "viva-winsign-"+hex.EncodeToString(random[:]))
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		// The extended path form supports both long local and UNC temp paths.
		if len(absolute) >= 248 && !strings.HasPrefix(absolute, `\\?\`) && strings.HasPrefix(absolute, `\\`) {
			absolute = `\\?\UNC\` + absolute[2:]
		} else if len(absolute) >= 248 && !strings.HasPrefix(absolute, `\\?\`) {
			absolute = `\\?\` + absolute
		}
		wide, err := syscall.UTF16PtrFromString(absolute)
		if err != nil {
			return "", err
		}
		err = syscall.CreateDirectory(wide, &attributes)
		runtime.KeepAlive(sddl)
		if err == nil {
			return path, nil
		}
		if err != syscall.ERROR_ALREADY_EXISTS {
			return "", &os.PathError{Op: "mkdir", Path: path, Err: err}
		}
	}
	return "", fmt.Errorf("creating private signing directory: %w", os.ErrExist)
}
