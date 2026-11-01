package judge

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"exam.local/monaco-examd/internal/store"
)

// ErrBusy 表示判题队列已满。
var ErrBusy = errors.New("judge queue busy")

type job struct {
	run store.JudgeRun
}

// Queue 是固定并发的判题队列。
type Queue struct {
	Judge *Judge
	Store *store.Store
	Log   *slog.Logger

	jobs chan job
	wg   sync.WaitGroup
}

// NewQueue 创建队列。
func NewQueue(j *Judge, st *store.Store, log *slog.Logger) *Queue {
	if log == nil {
		log = slog.Default()
	}
	return &Queue{Judge: j, Store: st, Log: log, jobs: make(chan job, 64)}
}

// Start 启动 n 个 worker，ctx 取消后停止取新任务。
func (q *Queue) Start(ctx context.Context, n int) {
	if n <= 0 {
		n = 1
	}
	for i := 0; i < n; i++ {
		q.wg.Add(1)
		go func() {
			defer q.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case j, ok := <-q.jobs:
					if !ok {
						return
					}
					q.execute(ctx, j)
				}
			}
		}()
	}
}

// Wait 等待所有 worker 退出。
func (q *Queue) Wait() { q.wg.Wait() }

// Submit 登记一次判题并入队。
func (q *Queue) Submit(ctx context.Context, studentNo, problemID string) (store.JudgeRun, error) {
	run := store.JudgeRun{
		ID:        store.NewID(),
		StudentNo: studentNo,
		Problem:   problemID,
		Status:    "queued",
		CreatedAt: time.Now().UTC(),
	}
	if err := q.Store.InsertJudgeRun(ctx, run); err != nil {
		return store.JudgeRun{}, err
	}
	select {
	case q.jobs <- job{run: run}:
		return run, nil
	default:
		run.Status = "failed"
		run.Detail = "judge queue full"
		run.FinishedAt = time.Now().UTC()
		_ = q.Store.UpdateJudgeRun(ctx, run)
		return run, ErrBusy
	}
}

func (q *Queue) execute(ctx context.Context, j job) {
	run := j.run
	run.Status = "running"
	if err := q.Store.UpdateJudgeRun(ctx, run); err != nil {
		q.Log.Warn("judge status update failed", "id", run.ID, "err", err)
	}
	res := q.Judge.Run(ctx, run)
	if err := q.Store.UpdateJudgeRun(ctx, res); err != nil {
		q.Log.Warn("judge result update failed", "id", res.ID, "err", err)
	}
	q.Log.Info("judge finished", "id", res.ID, "student", res.StudentNo, "problem", res.Problem,
		"status", res.Status, "score", res.Score, "total", res.Total)
}
