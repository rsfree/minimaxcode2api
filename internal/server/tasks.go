package server

import (
	"context"
	"errors"

	"github.com/linguo2625469/minimaxcode2api/internal/videoproto"
	"github.com/linguo2625469/minimaxcode2api/internal/videotask"
)

// RefreshTaskByID 主动同步一个视频任务的上游状态。
//
// 面板「任务」页用它把 running 推进到 succeeded/failed：客户端不轮询时，
// 任务会永远停在提交那一刻的状态，面板看到的和上游实际进度就脱节了。
// 终态直接返回（上游对终态任务也只会回同样的结果，白花一次调用）。
func (s *Server) RefreshTaskByID(ctx context.Context, id string) (*videotask.Record, error) {
	rec, ok := s.VideoTasks.Get(id)
	if !ok {
		return nil, videotask.ErrNotFound
	}
	if videoproto.IsTerminal(rec.Status) {
		return rec, nil
	}
	updated, perr := s.refreshVideoTask(ctx, rec)
	if perr != nil {
		return rec, errors.New(perr.Message)
	}
	return updated, nil
}

// DeleteTask 删除一条任务记录（只允许终态，与对外协议一致）。
func (s *Server) DeleteTask(id string) error {
	rec, ok := s.VideoTasks.Get(id)
	if !ok {
		return videotask.ErrNotFound
	}
	if !videoproto.IsTerminal(rec.Status) {
		return videotask.ErrNotTerminal
	}
	s.VideoTasks.Delete(id)
	return nil
}
