package vm

import (
	"fmt"
	"strings"
)

// virtualizationEntitlement is the codesign entitlement the Virtualization
// framework requires to create or start a VM. When br is built from source
// without `make sign`, the adhoc binary lacks it and VZ rejects the machine
// configuration.
const virtualizationEntitlement = "com.apple.security.virtualization"

// signingHint is the actionable guidance appended to VZ errors that stem from a
// missing Virtualization entitlement.
const signingHint = "br is not codesigned with the Virtualization entitlement " +
	"(" + virtualizationEntitlement + "); run `make sign` after `make build`, " +
	"or install via Homebrew (`brew install stuffbucket/tap/bladerunner`)"

// isMissingEntitlementError reports whether err is a Virtualization framework
// failure caused by the missing com.apple.security.virtualization entitlement.
//
// The Virtualization framework surfaces this as an NSError in the VZErrorDomain
// with code 2 (invalid configuration); the localized description names the
// entitlement. Matching on the entitlement string keeps this specific to the
// signing failure and avoids swallowing genuinely malformed configurations,
// while living in a build-tag-free file so it is exercised on CI-Linux too.
func isMissingEntitlementError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, strings.ToLower(virtualizationEntitlement)) {
		return true
	}
	// Fall back to the domain/keyword pairing the framework emits when the
	// localized description omits the full entitlement identifier.
	return strings.Contains(msg, "vzerrordomain") && strings.Contains(msg, "entitlement")
}

// annotateVZStartError wraps err with actionable signing guidance when it is a
// missing-entitlement failure, and returns err unchanged otherwise. Unrelated
// error paths are left intact.
func annotateVZStartError(err error) error {
	if !isMissingEntitlementError(err) {
		return err
	}
	return fmt.Errorf("%w\n\n%s", err, signingHint)
}
