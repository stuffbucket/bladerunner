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
		fmt.Print(bladerunner.License)
		fmt.Println()
		fmt.Print(bladerunner.Notice)
	},
}
