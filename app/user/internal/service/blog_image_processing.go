package service

import (
	"context"

	"cwxu-algo/app/common/blogimg"
)

const maxConcurrentBlogImageProcesses = 5

type blogImageProcessFunc func([]byte, string) (blogimg.CompressResult, error)

type blogImageProcessor struct {
	slots   chan struct{}
	process blogImageProcessFunc
}

func newBlogImageProcessor(limit int, process blogImageProcessFunc) *blogImageProcessor {
	return &blogImageProcessor{slots: make(chan struct{}, limit), process: process}
}

func (p *blogImageProcessor) Process(ctx context.Context, data []byte, contentType string) (blogimg.CompressResult, error) {
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	case <-ctx.Done():
		return blogimg.CompressResult{}, ctx.Err()
	}
	return p.process(data, contentType)
}

var globalBlogImageProcessor = newBlogImageProcessor(maxConcurrentBlogImageProcesses, blogimg.ValidateAndCompressForUpload)
