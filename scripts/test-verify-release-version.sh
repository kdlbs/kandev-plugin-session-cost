#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd "$(dirname "$0")/.." && pwd)
verify_script=$repo_dir/scripts/verify-release-version.sh
test_dir=$(mktemp -d)
trap 'rm -rf "$test_dir"' EXIT
base_version=$(sed -nE 's/^version: "([0-9]+\.[0-9]+\.[0-9]+)"$/\1/p' "$repo_dir/manifest.yaml")
wrong_version=999.999.999
[ "$wrong_version" != "$base_version" ] || wrong_version=999.999.998

make_fixture() {
	name=$1
	fixture=$test_dir/$name
	mkdir -p "$fixture"
	cp "$repo_dir/Makefile" "$repo_dir/manifest.yaml" "$fixture/"
}

expect_failure() {
	name=$1
	fixture=$2
	shift 2
	if (cd "$fixture" && sh "$verify_script" "$@") > "$test_dir/output" 2>&1; then
		printf 'expected release verification to reject %s\n' "$name" >&2
		exit 1
	fi
}

make_fixture valid
(cd "$test_dir/valid" && sh "$verify_script" "v$base_version")

expect_failure 'a tag that differs from manifest.yaml' "$test_dir/valid" "v$wrong_version"
expect_failure 'a non-version release tag' "$test_dir/valid" "release-$base_version"

make_fixture wrong-manifest
sed "s/^version: \"$base_version\"$/version: \"$wrong_version\"/" "$test_dir/wrong-manifest/manifest.yaml" > "$test_dir/wrong-manifest/manifest.next"
mv "$test_dir/wrong-manifest/manifest.next" "$test_dir/wrong-manifest/manifest.yaml"
expect_failure 'a manifest version that differs from Makefile' "$test_dir/wrong-manifest" "v$base_version"

make_fixture wrong-makefile
sed "s/^VERSION := $base_version$/VERSION := $wrong_version/" "$test_dir/wrong-makefile/Makefile" > "$test_dir/wrong-makefile/Makefile.next"
mv "$test_dir/wrong-makefile/Makefile.next" "$test_dir/wrong-makefile/Makefile"
expect_failure 'a Makefile version that differs from manifest.yaml' "$test_dir/wrong-makefile" "v$base_version"

make_archive() {
	fixture=$1
	name=$2
	version=$3
	id=$4
	mkdir -p "$fixture/archive"
	sed -e "s/^version: \"$base_version\"$/version: \"$version\"/" \
		-e "s/^id: \"kandev-session-cost\"$/id: \"$id\"/" \
		"$repo_dir/manifest.yaml" > "$fixture/archive/manifest.yaml"
	package_file=$(cd "$fixture" && make -s package-file)
	tar -czf "$fixture/$name" -C "$fixture/archive" manifest.yaml
	printf '%s\n' "$package_file"
}

make_fixture matching-package
matching_file=$(make_archive "$test_dir/matching-package" "kandev-session-cost-$base_version.tar.gz" "$base_version" kandev-session-cost)
(cd "$test_dir/matching-package" && sh "$verify_script" "v$base_version" "$matching_file")

make_fixture wrong-package-version
wrong_file=$(make_archive "$test_dir/wrong-package-version" "kandev-session-cost-$base_version.tar.gz" "$wrong_version" kandev-session-cost)
expect_failure 'an archive manifest version that differs from its tag' "$test_dir/wrong-package-version" "v$base_version" "$wrong_file"

make_fixture wrong-package-id
wrong_id_file=$(make_archive "$test_dir/wrong-package-id" "kandev-session-cost-$base_version.tar.gz" "$base_version" another-plugin)
expect_failure 'an archive manifest with another plugin id' "$test_dir/wrong-package-id" "v$base_version" "$wrong_id_file"

make_fixture wrong-package-name
wrong_name_file=$(make_archive "$test_dir/wrong-package-name" "renamed.tar.gz" "$base_version" kandev-session-cost)
expect_failure 'an archive whose filename differs from the package version' "$test_dir/wrong-package-name" "v$base_version" "$wrong_name_file"

printf 'release version negative tests passed\n'
