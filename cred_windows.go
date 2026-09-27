//go:build windows

package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/danieljoos/wincred"
)

// storeKind names the OS credential store in messages and source names.
const storeKind = "credential manager"

// credSave writes token under target in Windows Credential Manager. The write
// is an API call, so the token never appears on a command line the way the
// macOS keychain write has to avoid. A second write replaces the entry.
func credSave(target, token string) error {
	c := wincred.NewGenericCredential(target)
	c.CredentialBlob = []byte(token)
	c.Persist = wincred.PersistLocalMachine
	if err := c.Write(); err != nil {
		return fmt.Errorf("credential manager write failed: %w", err)
	}
	return nil
}

// credRead reads one Credential Manager entry. A missing entry is "" and no
// error. Any other failure is an error, so the Diagnostics tab can tell a
// broken store from an empty one.
func credRead(target string) (string, error) {
	c, err := wincred.GetGenericCredential(target)
	if errors.Is(err, wincred.ErrElementNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("credential manager read failed: %w", err)
	}
	return strings.TrimSpace(string(c.CredentialBlob)), nil
}
