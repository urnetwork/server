// `sidecarctl services apply-fragment <env> <fragment>... --out=<file>` merges the sidecar build's service
// blocks into the vault's services.yml, which nobody who writes them may read (plan-OPS-vault-blocks, QD-574;
// warp/services/sidecar-blocks/README.md). The owner runs it where the vault is.
//
// It reads <vault>/<env>/services.yml from the vault warpctl uses (warp_vault_home.go), applies the fragments
// in order as one new version on top of the file's versions, through warp's own merge, loader and port
// allocator (warp/services/fragment.go), and prints what each fragment added and the port blocks the new
// version assigns. warp's rules never let a block's port move, so a merge that would move one, change a
// value the file holds, or break a rule the loader checks is refused, and nothing is written. Otherwise the
// merged file goes to --out, which must not exist, for the owner to review and move into the vault: this
// never writes the vault itself. Fragments the file already holds add nothing, and then nothing is written.

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/urnetwork/warp/services"
)

// The command's registration. It is the only change to the binary this command needs.
func init() {
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "services apply-fragment",
		usage:   "<env> <fragment>... --out=<file>",
		summary: "Merge service block fragments (warp/services/sidecar-blocks/) into the vault's services.yml as one new top version, checked by warp's loader and port rules, and write the result to --out for review. The vault is never written.",
		run:     runServicesApplyFragment,
	})
}

// Parses the command line, finds the vault and applies the fragments.
func runServicesApplyFragment(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("services apply-fragment", flag.ContinueOnError)
	outPath := flags.String("out", "", "where the merged services.yml goes; it must not exist")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) < 2 {
		return sidecarctlUsageErrorf("want an env and at least one fragment")
	}
	if *outPath == "" {
		return sidecarctlUsageErrorf("want --out, where the merged services.yml goes")
	}
	vaultHome, err := findWarpVaultHome()
	if err != nil {
		return err
	}
	return applyServicesFragments(vaultHome, positionals[0], positionals[1:], *outPath, out)
}

// Applies the fragment files to env's services.yml in vaultHome and writes the result to outPath, printing
// what changed to out.
func applyServicesFragments(vaultHome string, env string, fragmentPaths []string, outPath string, out io.Writer) error {
	servicesConfigPath := services.ServicesConfigPath(vaultHome, env)
	servicesYaml, err := os.ReadFile(servicesConfigPath)
	if err != nil {
		return err
	}
	fragments := []*services.ServicesFragment{}
	for _, fragmentPath := range fragmentPaths {
		fragment, err := services.LoadServicesFragment(fragmentPath)
		if err != nil {
			return err
		}
		fragments = append(fragments, fragment)
	}
	result, err := services.ApplyServicesFragments(servicesYaml, servicesConfigPath, fragments...)
	if err != nil {
		return err
	}
	if !result.Changed() {
		fmt.Fprintf(out, "%s already holds every fragment; nothing written\n", servicesConfigPath)
		return nil
	}

	fmt.Fprintf(out, "the new top version of %s adds:\n", servicesConfigPath)
	for _, change := range result.Changes {
		fmt.Fprintf(out, "    %s\n", change)
	}
	if 0 < len(result.AddedPortBlocks) {
		fmt.Fprintf(out, "and assigns these ports, every earlier port staying where it was:\n")
		for _, portBlock := range result.AddedPortBlocks {
			fmt.Fprintf(out, "    %s\n", portBlock)
		}
	}

	file, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s exists, and this never writes over a file", outPath)
	}
	if err != nil {
		return err
	}
	if _, err := file.Write(result.Text); err != nil {
		file.Close()
		os.Remove(outPath)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(outPath)
		return err
	}
	fmt.Fprintf(out, "wrote %s; review it, then move it to %s\n", outPath, servicesConfigPath)
	return nil
}
