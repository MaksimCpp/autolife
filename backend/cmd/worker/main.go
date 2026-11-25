package main

import (
	"os"

	"autolife/backend/internal/platform/process"
)

func main() {
	os.Exit(process.Start("worker", "AUTOLIFE_WORKER_DATABASE_URL"))
}
