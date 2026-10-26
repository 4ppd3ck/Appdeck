//go:build !darwin

// Signature verification stubs for non-darwin decks: Windows and Linux have
// no launch-time code-seal equivalent (Windows SmartScreen rides quarantine
// the launcher's downloads never carry; Linux runs anything executable), so
// there is nothing to verify and nothing to repair. importSignatureState
// short-circuits before these are reached; they exist so the shared import
// code builds everywhere unchanged.
package main

import "errors"

var errNoCodesign = errors.New("code signatures are a macOS launch-time check — nothing to verify on this platform")

func verifyCodeSignature(string, *jobLog) error {
	return errNoCodesign
}

func adHocResign(string, bool, *jobLog) error {
	return errNoCodesign
}
