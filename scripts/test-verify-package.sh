#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd "$(dirname "$0")/.." && pwd)
verify_script=$repo_dir/scripts/verify-package.sh
test_dir=$(mktemp -d)
trap 'rm -rf "$test_dir"' EXIT

hash_file() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1"
	else
		shasum -a 256 "$1"
	fi
}

copy_fixture() {
	fixture=$test_dir/$1
	mkdir -p "$fixture/ui" "$fixture/server"
	cp "$repo_dir/manifest.yaml" "$fixture/manifest.yaml"
	cp "$repo_dir/ui/bundle.js" "$fixture/ui/bundle.js"
	awk '
		$0 == "runtime:" { in_runtime = 1; next }
		in_runtime && $0 == "  executables:" { in_executables = 1; next }
		in_executables && $0 !~ /^    / { exit }
		in_executables && /^    [[:alnum:]_-]+: "[^"]+"$/ {
			path = $2
			gsub(/"/, "", path)
			print path
		}
	' "$repo_dir/manifest.yaml" | while IFS= read -r path; do
		mkdir -p "$fixture/$(dirname "$path")"
		printf 'fixture executable\n' > "$fixture/$path"
		chmod +x "$fixture/$path"
	done
	write_checksums "$fixture"
}

write_checksums() {
	fixture=$1
	rm -f "$fixture/checksums.txt"
	(cd "$fixture" && find . -type f ! -name checksums.txt -print | sed 's#^\./##' | LC_ALL=C sort) > "$test_dir/files"
	while IFS= read -r path; do
		(cd "$fixture" && hash_file "$path") >> "$fixture/checksums.txt"
	done < "$test_dir/files"
}

expect_failure() {
	name=$1
	fixture=$2
	shift 2
	if sh "$verify_script" "$fixture" "$@" > "$test_dir/output" 2>&1; then
		printf 'expected package verification to reject %s\n' "$name" >&2
		exit 1
	fi
}

copy_fixture valid
sh "$verify_script" "$test_dir/valid" full >/dev/null

copy_fixture valid-host
rm "$test_dir/valid-host/server/plugin-darwin-amd64"
rm "$test_dir/valid-host/server/plugin-darwin-arm64"
rm "$test_dir/valid-host/server/plugin-linux-arm64"
rm "$test_dir/valid-host/server/plugin-windows-amd64.exe"
write_checksums "$test_dir/valid-host"
sh "$verify_script" "$test_dir/valid-host" host linux-amd64 >/dev/null

# Current plugin-pack emits plain scalar values and four-space indentation.
copy_fixture normalized-full
# Increase all runtime indentation without relying on awk replacement groups.
sed -e 's/^id: "kandev-session-cost"$/id: kandev-session-cost/' \
	-e '/^runtime:/,/^[^ ]/{ s/^  /    /; s/^      /        /; s/"//g; }' \
	"$test_dir/normalized-full/manifest.yaml" > "$test_dir/normalized-full/manifest.next"
mv "$test_dir/normalized-full/manifest.next" "$test_dir/normalized-full/manifest.yaml"
write_checksums "$test_dir/normalized-full"
sh "$verify_script" "$test_dir/normalized-full" full >/dev/null

copy_fixture normalized-host
cp "$test_dir/normalized-full/manifest.yaml" "$test_dir/normalized-host/manifest.yaml"
sed -e '/        darwin-/d' -e '/        linux-arm64:/d' -e '/        windows-/d' \
	"$test_dir/normalized-host/manifest.yaml" > "$test_dir/normalized-host/manifest.next"
mv "$test_dir/normalized-host/manifest.next" "$test_dir/normalized-host/manifest.yaml"
rm "$test_dir/normalized-host/server/plugin-darwin-amd64" \
	"$test_dir/normalized-host/server/plugin-darwin-arm64" \
	"$test_dir/normalized-host/server/plugin-linux-arm64" \
	"$test_dir/normalized-host/server/plugin-windows-amd64.exe"
write_checksums "$test_dir/normalized-host"
sh "$verify_script" "$test_dir/normalized-host" host linux-amd64 >/dev/null
expect_failure 'a host-only manifest in a full package' "$test_dir/normalized-host" full
expect_failure 'a host-only manifest for another platform' "$test_dir/normalized-host" host linux-arm64

copy_fixture missing-binary
rm "$test_dir/missing-binary/server/plugin-linux-amd64"
write_checksums "$test_dir/missing-binary"
expect_failure 'a missing declared platform binary' "$test_dir/missing-binary" full

copy_fixture unexpected-file
printf 'unexpected\n' > "$test_dir/unexpected-file/extra.txt"
write_checksums "$test_dir/unexpected-file"
expect_failure 'an unexpected checksummed file' "$test_dir/unexpected-file" full

copy_fixture corrupt-file
printf 'changed after hashing\n' >> "$test_dir/corrupt-file/ui/bundle.js"
expect_failure 'a file whose checksum does not match' "$test_dir/corrupt-file" full

copy_fixture missing-checksum
sed '$d' "$test_dir/missing-checksum/checksums.txt" > "$test_dir/missing-checksum/checksums.next"
mv "$test_dir/missing-checksum/checksums.next" "$test_dir/missing-checksum/checksums.txt"
expect_failure 'an omitted checksum entry' "$test_dir/missing-checksum" full

copy_fixture duplicate-checksum
duplicate_line=$(sed -n '1p' "$test_dir/duplicate-checksum/checksums.txt")
printf '%s\n' "$duplicate_line" >> "$test_dir/duplicate-checksum/checksums.txt"
expect_failure 'a duplicate checksum entry' "$test_dir/duplicate-checksum" full

copy_fixture wrong-plugin-id
sed 's/^id: "kandev-session-cost"$/id: "another-plugin"/' "$test_dir/wrong-plugin-id/manifest.yaml" > "$test_dir/wrong-plugin-id/manifest.next"
mv "$test_dir/wrong-plugin-id/manifest.next" "$test_dir/wrong-plugin-id/manifest.yaml"
write_checksums "$test_dir/wrong-plugin-id"
expect_failure 'a manifest with another plugin id' "$test_dir/wrong-plugin-id" full

copy_fixture wrong-platform-set
sed 's/^    windows-amd64:/    freebsd-amd64:/' "$test_dir/wrong-platform-set/manifest.yaml" > "$test_dir/wrong-platform-set/manifest.next"
mv "$test_dir/wrong-platform-set/manifest.next" "$test_dir/wrong-platform-set/manifest.yaml"
write_checksums "$test_dir/wrong-platform-set"
expect_failure 'a manifest with an undeclared platform' "$test_dir/wrong-platform-set" full

expect_failure 'an unsupported host platform' "$test_dir/valid" host freebsd-amd64

for quote_side in opening closing; do
	for field in id executable; do
		name=unmatched-$quote_side-$field
		copy_fixture "$name"
		case "$field:$quote_side" in
			id:opening) edit='s/^id: "kandev-session-cost"$/id: "kandev-session-cost/' ;;
			id:closing) edit='s/^id: "kandev-session-cost"$/id: kandev-session-cost"/' ;;
			executable:opening) edit='s#linux-amd64: "server/plugin-linux-amd64"#linux-amd64: "server/plugin-linux-amd64#' ;;
			executable:closing) edit='s#linux-amd64: "server/plugin-linux-amd64"#linux-amd64: server/plugin-linux-amd64"#' ;;
		esac
		sed "$edit" "$fixture/manifest.yaml" > "$fixture/manifest.next"
		mv "$fixture/manifest.next" "$fixture/manifest.yaml"
		write_checksums "$fixture"
		expect_failure "an unmatched $quote_side quote in $field" "$fixture" full
	done
done

printf 'package verifier negative tests passed\n'
