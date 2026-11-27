package imageflow

import "autolife/backend/internal/fileflow/processing"

type Launcher = processing.Launcher

func NewLauncher(socket string) (*Launcher, error) {
	return processing.NewLauncher(socket, "image-transform")
}
