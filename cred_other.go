//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// storeKind names the OS credential store in messages and source names. Off
// Windows the only store clusage writes is the macOS keychain.
const storeKind = "keychain"

// credSave stores token under target in the login keychain.
//
// The token goes in over stdin rather than as a "-w <token>" argument. Command
// arguments are visible to any local process running ps for as long as the
// command runs, so an argument would expose the token to every other user on
// the machine. With -w last it prompts for the value and a confirmation, so the
// token is written twice.
func credSave(target, token string) error {
	cmd := exec.Command("security", "add-generic-password",
		"-a", os.Getenv("USER"), "-s", target, "-U", "-w")
	cmd.Stdin = strings.NewReader(token + "\n" + token + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("keychain write failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// credPassword reads one keychain entry, or "" when it is missing.
func credPassword(target string) string {
	return keychainPassword(target)
}
