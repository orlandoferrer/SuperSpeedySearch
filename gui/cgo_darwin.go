//go:build darwin

package main

// Wails v2 references UTType on modern macOS SDKs but does not link the
// framework that provides it; without this, `go build` fails with
// "_OBJC_CLASS_$_UTType" undefined.

/*
#cgo LDFLAGS: -framework UniformTypeIdentifiers
*/
import "C"
