# Windows token store

Status: done 2026-09-27. See the revisit note below for the follow-up.

## Goal

Store the OAuth token from `clusage setup` in Windows Credential Manager.
Before this work, `setup` wrote only to the macOS keychain.

## Approach

Use `github.com/danieljoos/wincred`. It wraps the CredWrite and CredRead APIs
in pure Go, so the release build stays free of cgo. The entry name stays
`clusage` on both platforms. `cmdkey` was rejected: it writes a credential but
cannot read one back.

## Revisit: replace both stores with go-keyring

This work is a stopgap. Redo it with `github.com/zalando/go-keyring` later.
That library covers the macOS keychain, Windows Credential Manager and Linux
Secret Service behind one API. It would replace the `security` CLI calls and
the wincred code with one store package.

Why not now: the swap touches the working macOS path, and it adds a dbus
dependency on Linux. Do it as its own refactor when Linux token storage
becomes a goal.
