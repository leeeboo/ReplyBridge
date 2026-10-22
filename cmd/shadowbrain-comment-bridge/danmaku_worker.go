package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"path/filepath"

	"comment-bridge/internal/codexreview"
	"comment-bridge/internal/commentbridge"
)

func runDanmakuWorker(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("review-danmaku", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("queue-dir", "", "受限的本地弹幕队列目录")
	binary := fs.String("codex-bin", "", "宿主机 Codex CLI 路径")
	if fs.Parse(args) != nil || fs.NArg() != 0 || !filepath.IsAbs(*binary) {
		return errors.New("review-danmaku 参数无效")
	}
	queue, err := codexreview.NewQueue(*dir)
	if err != nil {
		return errors.New("弹幕分析队列目录无效")
	}
	if err := queue.WorkOnce(ctx, func(ctx context.Context, items []commentbridge.Danmaku) (commentbridge.DanmakuReview, error) {
		return codexreview.Review(ctx, *binary, items)
	}); err != nil {
		return errors.New("弹幕分析任务处理失败")
	}
	return nil
}
