#!/usr/bin/env bash
# Provisioning for the Go toolchain that builds the signed, notarized app.
#
# A signed release is only as trustworthy as the compiler that produced it, so a
# toolchain this script downloads is verified against a digest pinned in the
# repository. The pin file is the trust anchor: the builder is self-hosted, and
# anything on its filesystem could be rewritten by whatever rewrote the cache,
# whereas a digest changes only through a reviewed commit.
#
# Sourced by build.sh, and by the tests that exercise it. It defines functions
# and runs nothing on its own.

# go_pinned_digest echoes the reviewed SHA-256 for an archive.
#
# An archive with no pin fails rather than downloading unverified. An unknown
# compiler building a signed release is the outcome this exists to prevent, and
# falling back to "download it anyway" would restore exactly that.
go_pinned_digest() {
    local pin_file="$1" archive="$2" digest
    if [[ ! -f "${pin_file}" ]]; then
        echo "::error::pin file ${pin_file} is missing; refusing to provision an unverified toolchain" >&2
        return 1
    fi
    digest="$(awk -v want="${archive}" '$2 == want {print $1; exit}' "${pin_file}")"
    if [[ -z "${digest}" ]]; then
        echo "::error::no pinned SHA-256 for ${archive} in ${pin_file}" >&2
        echo "::error::add the digest from https://go.dev/dl/?mode=json in a reviewed commit" >&2
        return 1
    fi
    printf '%s' "${digest}"
}

# go_sha256 echoes a file's SHA-256, using whichever tool the host provides.
go_sha256() {
    local file="$1"
    if command -v shasum >/dev/null 2>&1; then
        shasum -a 256 -- "${file}" | awk '{print $1}'
    elif command -v sha256sum >/dev/null 2>&1; then
        sha256sum -- "${file}" | awk '{print $1}'
    else
        echo "::error::neither shasum nor sha256sum is available; cannot verify the toolchain" >&2
        return 1
    fi
}

# go_verify_digest checks a file against an expected SHA-256.
go_verify_digest() {
    local file="$1" want="$2" got
    got="$(go_sha256 "${file}")" || return 1
    if [[ "${got}" != "${want}" ]]; then
        echo "::error::digest mismatch for ${file}" >&2
        echo "::error::  expected ${want}" >&2
        echo "::error::  actual   ${got}" >&2
        return 1
    fi
}

# go_fetch_archive downloads to a temporary file, verifies it, and only then
# moves it to its cached name.
#
# Verifying before the archive reaches that name is what stops an interrupted or
# tampered download from being mistaken for a good one on a later run: a partial
# file never gets the name a later run would trust.
go_fetch_archive() {
    local url="$1" dest="$2" want="$3" tmp
    tmp="$(mktemp "${dest}.XXXXXX")" || return 1
    echo "Downloading ${url}" >&2
    if ! curl --fail --location --silent --show-error --output "${tmp}" "${url}"; then
        rm -f -- "${tmp}"
        return 1
    fi
    if ! go_verify_digest "${tmp}" "${want}"; then
        rm -f -- "${tmp}"
        return 1
    fi
    mv -f -- "${tmp}" "${dest}"
}

# go_provision installs a verified Go toolchain and echoes its bin directory.
#
# Usage: go_provision <version> <cache_root> <pin_file> <base_url> <platform>
#
# The archive is cached; the extracted tree is not trusted between runs and is
# rebuilt from the verified archive every time. A checksum recorded beside an
# extracted tree proves nothing — anything able to alter the `go` binary could
# alter that record too. Re-extracting costs seconds, and the download still
# happens only once.
go_provision() {
    local ver="$1" cache_root="$2" pin_file="$3" base_url="$4" platform="$5"
    local archive cache archive_path want staging

    archive="go${ver}.${platform}.tar.gz"
    cache="${cache_root}/${ver}"
    archive_path="${cache}/${archive}"

    want="$(go_pinned_digest "${pin_file}" "${archive}")" || return 1
    mkdir -p "${cache}" || return 1

    # A cached archive is re-verified every run, so a tampered cache is detected
    # and replaced rather than reused.
    if [[ -f "${archive_path}" ]] && ! go_verify_digest "${archive_path}" "${want}"; then
        echo "cached ${archive} failed verification; discarding it" >&2
        rm -f -- "${archive_path}"
    fi
    if [[ ! -f "${archive_path}" ]]; then
        go_fetch_archive "${base_url}/${archive}" "${archive_path}" "${want}" || return 1
    fi

    # Extract into a fresh directory and swap it in, so an interrupted
    # extraction cannot leave a half-populated toolchain the next run would use.
    staging="$(mktemp -d "${cache}/staging.XXXXXX")" || return 1
    if ! tar -xzf "${archive_path}" -C "${staging}"; then
        rm -rf -- "${staging}"
        return 1
    fi
    if [[ ! -x "${staging}/go/bin/go" ]]; then
        echo "::error::${archive} does not contain go/bin/go" >&2
        rm -rf -- "${staging}"
        return 1
    fi
    rm -rf -- "${cache}/go.previous"
    [[ -d "${cache}/go" ]] && mv -- "${cache}/go" "${cache}/go.previous"
    mv -- "${staging}/go" "${cache}/go"
    rm -rf -- "${staging}" "${cache}/go.previous"

    # Read by build.sh after sourcing this library, to record which compiler
    # built the signed app and where it came from.
    # shellcheck disable=SC2034  # consumed by the sourcing script, not here.
    GO_TOOLCHAIN_SOURCE="${base_url}/${archive}"
    # shellcheck disable=SC2034  # consumed by the sourcing script, not here.
    GO_TOOLCHAIN_DIGEST="${want}"
    printf '%s' "${cache}/go/bin"
}
