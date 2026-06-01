package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/stuffbucket/bladerunner"
)

var noticeCmd = &cobra.Command{
	Use:   "notice",
	Short: "Print license and third-party notices",
	Long:  "Display the project license and third-party software attributions compiled into this binary.",
	Run: func(_ *cobra.Command, _ []string) {
		if jsonOutput {
			// Mirror the human layout: license, a blank line, then the notice.
			_ = emitJSON(map[string]string{
				"notice": bladerunner.License + "\n" + bladerunner.Notice,
			})
			return
		}
		fmt.Print(bladerunner.License)
		fmt.Println()
		fmt.Print(bladerunner.Notice)
	},
}
