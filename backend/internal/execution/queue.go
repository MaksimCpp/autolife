package execution

import (
	"context"
	"time"

	"autolife/backend/internal/workflow"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

func New(pool *pgxpool.Pool, r *workflow.Registry, consume bool) (*Store, error) {
	result := &Store{Pool: pool, Registry: r}
	config := &river.Config{Schema: "queue"}
	if consume {
		workers := river.NewWorkers()
		if err := river.AddWorkerSafely(workers, &Worker{Store: result}); err != nil {
			return nil, err
		}
		config.Workers = workers
		config.Queues = map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 2}}
		config.JobTimeout = 16 * time.Minute
		config.RescueStuckJobsAfter = 20 * time.Minute
	}
	queue, err := river.NewClient(riverpgxv5.New(pool), config)
	if err != nil {
		return nil, err
	}
	result.Queue = queue
	return result, nil
}
func (s *Store) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.Queue.StopAndCancel(ctx)
}
