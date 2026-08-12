// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package cmdapi

import (
	"fmt"
	"strings"

	"golang.org/x/mod/semver"
)

const (
	versionFmt  = "atlas community "
	versionInfo = "Fork releases: " + ForkReleasesURL + "\n"

	// forkSuffix prefixes the pre-release part of every version built from this
	// fork, e.g. "v1.3.0-views-ce.3". It is what tells a fork build apart from an
	// upstream one, since both are stamped into the same "version" variable.
	forkSuffix = "-views-ce"

	// ForkReleasesURL is the releases page of this fork.
	ForkReleasesURL = "https://github.com/torchsecurity/atlas/releases"
)

// IsForkVersion reports whether version was stamped by a release of this fork.
// Development builds (an unset or non-semver version) and upstream versions are
// not: they carry no fork suffix.
func IsForkVersion(version string) bool {
	return semver.IsValid(version) && strings.HasPrefix(semver.Prerelease(version), forkSuffix)
}

// forkReleaseURL returns the release page of the given fork version.
func forkReleaseURL(version string) string {
	return fmt.Sprintf("%s/tag/%s", ForkReleasesURL, version)
}
