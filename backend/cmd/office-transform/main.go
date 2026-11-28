package main

import (
	"os"

	"autolife/backend/internal/pdfcheck"
)

func main() {
	if len(os.Args) != 2 || pdfcheck.OutputPages(os.Args[1]) == 0 {
		os.Exit(20)
	}
}
