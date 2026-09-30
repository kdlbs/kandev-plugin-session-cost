#!/bin/sh
set -eu

fail() {
	printf 'release version verification failed: %s\n' "$1" >&2
	exit 1
}

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
	fail 'usage: verify-release-version.sh TAG [PACKAGE_FILE]'
fi

tag=$1
if ! printf '%s\n' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
	fail "tag is not a release version: $tag"
fi

tag_version=${tag#v}
manifest_version=$(sed -nE 's/^version: "([0-9]+\.[0-9]+\.[0-9]+)"$/\1/p' manifest.yaml)
make_version=$(sed -nE 's/^VERSION := ([0-9]+\.[0-9]+\.[0-9]+)$/\1/p' Makefile)
[ -n "$manifest_version" ] || fail 'manifest.yaml has no SemVer version'
[ -n "$make_version" ] || fail 'Makefile has no SemVer VERSION'
[ "$manifest_version" = "$make_version" ] || fail "manifest version $manifest_version differs from Makefile version $make_version"
[ "$tag_version" = "$manifest_version" ] || fail "tag $tag differs from manifest version $manifest_version"

if [ "$#" -eq 2 ]; then
	package_file=$2
	expected_package=$(make -s package-file)
	[ -f "$package_file" ] || fail "package file not found: $package_file"
	[ "$(basename "$package_file")" = "$expected_package" ] || fail "package filename $(basename "$package_file") differs from $expected_package"
	package_id=$(tar -xOzf "$package_file" manifest.yaml | sed -nE 's/^id: "([^"]+)"$/\1/p') || fail 'cannot read package manifest'
	package_version=$(tar -xOzf "$package_file" manifest.yaml | sed -nE 's/^version: "([0-9]+\.[0-9]+\.[0-9]+)"$/\1/p') || fail 'cannot read package manifest version'
	[ "$package_id" = "kandev-session-cost" ] || fail "package id ${package_id:-missing} differs from kandev-session-cost"
	[ "$package_version" = "$tag_version" ] || fail "package manifest version ${package_version:-missing} differs from tag $tag"
fi
