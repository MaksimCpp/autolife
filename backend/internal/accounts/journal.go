package accounts

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type Journal struct{ directory string }

func NewJournal(directory string) (Journal, error) {
	if !filepath.IsAbs(directory) {
		return Journal{}, errors.New("AUTOLIFE_RESTORE_JOURNAL_DIR must be an absolute directory")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return Journal{}, errors.New("cannot initialize restore journal directory")
	}
	return Journal{directory: directory}, nil
}

func (j Journal) append(id, owner string, requested time.Time) error {
	if j.directory == "" || !validID(id) || !validID(owner) {
		return errors.New("invalid restore journal record")
	}
	record, err := json.Marshal(struct {
		ID          string    `json:"request_id"`
		Owner       string    `json:"owner_id"`
		RequestedAt time.Time `json:"requested_at"`
	}{id, owner, requested.UTC()})
	if err != nil {
		return err
	}
	target := filepath.Join(j.directory, id+".json")
	tmp, err := os.CreateTemp(j.directory, ".entry-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(record); err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Link(tmp.Name(), target); errors.Is(err, os.ErrExist) {
		data, err := os.ReadFile(target)
		if err != nil || !bytes.Equal(record, data) {
			return errors.New("restore journal record does not match")
		}
	} else if err != nil {
		return err
	}
	dir, err := os.Open(j.directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
