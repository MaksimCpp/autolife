package main

import (
	"os"

	"autolife/backend/internal/platform/process"
)

func main() {
	os.Exit(process.Start("api", "AUTOLIFE_API_DATABASE_URL"))
}
