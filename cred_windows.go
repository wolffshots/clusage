//go:build windows

package main

import "github.com/danieljoos/wincred"

// storeKind names the OS credential store in messages and source names.
const storeKind = "credential manager"

// credSave writes token under target in Windows Credential Manager. The write
// is an API call, so the token never appears on a command line the way the
// macOS keychain write has to avoid.
func credSave(target, token string) error {
	c := wincred.NewGenericCredential(target)
	c.CredentialBlob = []byte(token)
	c.Persist = wincred.PersistLocalMachine
	return c.Write()
}

// credPassword reads one Credential Manager entry, or "" when it is missing.
func credPassword(target string) string {
	c, err := wincred.GetGenericCredential(target)
	if err != nil {
		return ""
	}
	return string(c.CredentialBlob)
}
