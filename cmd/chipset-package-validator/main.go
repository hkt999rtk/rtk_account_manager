package main

import (
	"fmt"
	"os"

	"rtk_account_manager/internal/api"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: chipset-package-validator <package.json> [package.json ...]")
		os.Exit(2)
	}
	for _, path := range os.Args[1:] {
		raw, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			os.Exit(1)
		}
		if err := api.ValidateChipsetResourcePackage(raw); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			os.Exit(1)
		}
		fmt.Printf("%s: valid\n", path)
	}
}
