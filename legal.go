package bladerunner

import _ "embed"

// License is the project's own MIT license text, compiled into the binary so
// `br notice` can print it on a machine with no source checkout.
//
//go:embed LICENSE
var License string

// Notice is the third-party attribution text the licenses of bladerunner's
// dependencies require to be shipped with the binary. `br notice` prints it
// after License. It is a checked-in file, not generated at build time, so a new
// dependency must be added to it by hand.
//
//go:embed NOTICE
var Notice string
