// Command update-manifest builds latest.json, the manifest that `br self-update`
// fetches, from a GitHub release list.
//
// It is the pure half of the publish pipeline: it reads a release document on
// stdin and writes a manifest on stdout, so every decision it makes is covered
// by the tests of internal/update. The GitHub and network work around it belongs
// to .github/workflows/pages.yml, which generates the manifest into the site
// build output on every deploy. Nothing commits the result.
//
// Usage:
//
//	gh api repos/OWNER/REPO/releases --paginate > releases.json
//
//	# Print the tag whose release can serve an update, or nothing.
//	update-manifest select < releases.json
//
//	# Print that tag only if one named release can serve an update, which is how
//	# a caller waits for its asynchronously uploaded signed assets.
//	update-manifest select -tag v0.4.9 < releases.json
//
//	# Print the name of the signature asset to download from that release.
//	update-manifest signature-name
//
//	# Write the manifest for that release. When the release is older than the
//	# manifest already published, the published one is re-emitted instead.
//	update-manifest emit -signature Bladerunner.app.tar.gz.sig \
//	    -live-manifest published/latest.json < releases.json
//
// Exit codes: 0 success, 1 failure, 2 usage.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/stuffbucket/bladerunner/internal/update"
)

// Exit codes.
const (
	exitFailure = 1
	exitUsage   = 2
)

// manifestIndent is the indentation of the emitted latest.json. The file is
// small and people read it in a browser, so it is pretty-printed.
const manifestIndent = "  "

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: update-manifest select|signature-name|emit [flags] < releases.json")
		os.Exit(exitUsage)
	}
	var err error
	switch os.Args[1] {
	case "select":
		err = runSelectCmd(os.Args[2:], os.Stdin, os.Stdout)
	case "signature-name":
		err = runSignatureName(os.Stdout)
	case "emit":
		err = runEmit(os.Args[2:], os.Stdin, os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "update-manifest: unknown command %q\n", os.Args[1])
		os.Exit(exitUsage)
	}
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "update-manifest: %v\n", err)
	os.Exit(exitFailure)
}

// readReleases decodes a GitHub release list from r.
func readReleases(r io.Reader) ([]update.Release, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read release list: %w", err)
	}
	var releases []update.Release
	if err := json.Unmarshal(raw, &releases); err != nil {
		return nil, fmt.Errorf("parse release list: %w", err)
	}
	return releases, nil
}

// runSelectCmd parses the flags of the select command and runs it.
func runSelectCmd(args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("select", flag.ContinueOnError)
	tag := fs.String("tag", "", "consider only this release tag")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runSelect(in, out, *tag)
}

// runSelect prints the tag of the release that can serve an update. When
// onlyTag is set, only that one release is considered, which answers "is this
// release ready yet?" while its signed assets are still being uploaded.
//
// When no release qualifies it prints nothing and succeeds: an unpublished
// update channel is the ordinary state of a repository that has not shipped a
// signed bundle, not a failure.
func runSelect(in io.Reader, out io.Writer, onlyTag string) error {
	releases, err := readReleases(in)
	if err != nil {
		return err
	}
	rel, err := update.SelectRelease(withTag(releases, onlyTag))
	if errors.Is(err, update.ErrNoUpdaterRelease) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, rel.TagName)
	return err
}

// withTag narrows releases to the one named tag. An empty tag keeps them all.
func withTag(releases []update.Release, tag string) []update.Release {
	if tag == "" {
		return releases
	}
	var kept []update.Release
	for _, r := range releases {
		if r.TagName == tag {
			kept = append(kept, r)
		}
	}
	return kept
}

// runSignatureName prints the file name of the signature asset to download.
// The site build needs the name before it can fetch the file, and this keeps it
// from being written a second time in the workflow where nothing would notice
// it going stale.
func runSignatureName(out io.Writer) error {
	_, err := fmt.Fprintln(out, update.UpdaterSignatureName)
	return err
}

// runEmit writes the manifest for the selected release. It selects from the
// same release list runSelect was given, so the two agree by construction.
//
// When the selected release is older than the manifest that is already
// published, it re-emits the published one instead. Refusing that way keeps the
// caller's deploy simple and safe: whatever this writes is what should be live,
// so a rollback of a release cannot regress the update channel and cannot fail
// a site deploy that had nothing to do with the release.
func runEmit(args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("emit", flag.ContinueOnError)
	sigPath := fs.String("signature", "", "path to the downloaded Bladerunner.app.tar.gz.sig file")
	livePath := fs.String("live-manifest", "", "path to the manifest that is currently published")
	allowDowngrade := fs.Bool("allow-downgrade", false, "publish even when the candidate is older than the published one")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sigPath == "" {
		return errors.New("emit: -signature is required")
	}

	releases, err := readReleases(in)
	if err != nil {
		return err
	}
	rel, err := update.SelectRelease(releases)
	if err != nil {
		return err
	}
	sigFile, err := os.ReadFile(*sigPath)
	if err != nil {
		return fmt.Errorf("read signature file: %w", err)
	}
	m, err := update.BuildManifest(rel, sigFile)
	if err != nil {
		return err
	}

	if live, ok := publishedManifest(*livePath); ok {
		err := update.CheckDowngrade(m.Version, live.Version)
		switch {
		case err == nil:
		case !errors.Is(err, update.ErrDowngrade):
			return err
		case *allowDowngrade:
			fmt.Fprintf(os.Stderr, "update-manifest: publishing anyway (-allow-downgrade): %v\n", err)
		default:
			fmt.Fprintf(os.Stderr, "update-manifest: keeping the published manifest: %v\n", err)
			m = live
		}
	}

	enc := json.NewEncoder(out)
	enc.SetIndent("", manifestIndent)
	if err := enc.Encode(m); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

// publishedManifest reads the manifest that is currently published. An absent
// or unreadable file reports false: there is nothing to protect, so the caller
// publishes over it rather than wedging the update channel on a broken file.
func publishedManifest(path string) (*update.Manifest, bool) {
	if path == "" {
		return nil, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var m update.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false
	}
	if m.Version == "" {
		return nil, false
	}
	return &m, true
}
