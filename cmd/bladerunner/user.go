package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner/internal/oidc"
)

var userCmd = &cobra.Command{
	Use:   "user",
	Short: "Manage SSH-key identities recognized by the local OIDC provider",
	Long: `Manage the SSH public keys that bladerunner's local OIDC provider treats
as identities. Each registered key receives a SHA-256 fingerprint that becomes
the JWT subject when bladerunner exchanges it for an Incus access token.

Identity files live in $XDG_CONFIG_HOME/bladerunner/identities/ (default:
~/.config/bladerunner/identities/).`,
}

var userListCmd = &cobra.Command{
	Use:   "list",
	Short: "List registered identities",
	RunE:  runUserList,
}

var userAddCmd = &cobra.Command{
	Use:   "add <path-to-pubkey>",
	Short: "Register an SSH public key as an identity",
	Args:  cobra.ExactArgs(1),
	RunE:  runUserAdd,
}

var userRemoveCmd = &cobra.Command{
	Use:     "remove <fingerprint>",
	Aliases: []string{"rm"},
	Short:   "Revoke a registered identity",
	Args:    cobra.ExactArgs(1),
	RunE:    runUserRemove,
}

func init() {
	userCmd.AddCommand(userListCmd, userAddCmd, userRemoveCmd)
}

func openStore() (*oidc.Store, error) {
	dir := oidc.DefaultIdentityDir()
	store := oidc.NewStore(dir)
	if err := store.Load(); err != nil {
		return nil, fmt.Errorf("load identities: %w", err)
	}
	return store, nil
}

func runUserList(_ *cobra.Command, _ []string) error {
	store, err := openStore()
	if err != nil {
		return err
	}
	idents := store.List()
	if len(idents) == 0 {
		fmt.Println(subtle("No identities registered."))
		fmt.Printf("Add one with %s\n", command("br user add <pubkey>"))
		return nil
	}
	fmt.Println(title("Registered Identities"))
	fmt.Println()
	for _, ident := range idents {
		fmt.Printf("  %s\n", value(ident.Fingerprint))
		if ident.Comment != "" {
			fmt.Printf("    %s %s\n", key("label:"), ident.Comment)
		}
		if ident.Path != "" {
			fmt.Printf("    %s  %s\n", key("path:"), ident.Path)
		}
	}
	fmt.Println()
	fmt.Printf("Directory: %s\n", subtle(store.Dir()))
	return nil
}

func runUserAdd(_ *cobra.Command, args []string) error {
	store, err := openStore()
	if err != nil {
		return err
	}
	path := args[0]
	if !strings.HasSuffix(path, ".pub") {
		fmt.Println(subtle("Note: expected a .pub file; continuing anyway"))
	}
	ident, err := store.AddFromFile(path)
	if err != nil {
		return fmt.Errorf("add identity: %w", err)
	}
	fmt.Printf("%s Registered identity %s\n", success("✓"), value(ident.Fingerprint))
	if ident.Comment != "" {
		fmt.Printf("  %s %s\n", key("label:"), ident.Comment)
	}
	return nil
}

func runUserRemove(_ *cobra.Command, args []string) error {
	store, err := openStore()
	if err != nil {
		return err
	}
	fp := args[0]
	removed, err := store.Remove(fp)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("no identity with fingerprint %s", fp)
	}
	fmt.Printf("%s Removed identity %s\n", success("✓"), value(fp))
	return nil
}
