// The image launcher invokes this entrypoint. Validation precedes librsvg.
package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"

	"autolife/backend/internal/svgcheck"
)

func main() {
	var header [18]byte
	if _, err := io.ReadFull(os.Stdin, header[:]); err != nil {
		os.Exit(20)
	}
	var body io.Reader = os.Stdin
	switch string(header[:4]) {
	case "ALI1":
	case "ALS1":
		data, err := io.ReadAll(io.LimitReader(body, svgcheck.MaxBytes+1))
		if err != nil || svgcheck.Validate(bytes.NewReader(data)) != nil {
			os.Exit(20)
		}
		body = bytes.NewReader(data)
	default:
		os.Exit(20)
	}
	cmd := exec.Command("/usr/local/bin/image-native")
	cmd.Stdin = io.MultiReader(bytes.NewReader(header[:]), body)
	cmd.Stdout = os.Stdout
	if err := cmd.Run(); err != nil {
		if status, ok := err.(*exec.ExitError); ok && status.ExitCode() == 20 {
			os.Exit(20)
		}
		os.Exit(1)
	}
}
