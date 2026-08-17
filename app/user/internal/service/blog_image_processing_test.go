package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"cwxu-algo/app/common/blogimg"
)

func TestBlogImageProcessorLimitsConcurrencyToFive(t *testing.T) {
	entered := make(chan struct{}, 6)
	release := make(chan struct{})
	processor := newBlogImageProcessor(5, func([]byte, string) (blogimg.CompressResult, error) {
		entered <- struct{}{}
		<-release
		return blogimg.CompressResult{}, nil
	})

	done := make(chan error, 6)
	for i := 0; i < 6; i++ {
		go func() {
			_, err := processor.Process(context.Background(), nil, "image/png")
			done <- err
		}()
	}
	for i := 0; i < 5; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("first five processors did not enter")
		}
	}
	select {
	case <-entered:
		t.Fatal("sixth processor entered before a slot was released")
	case <-time.After(40 * time.Millisecond):
	}
	close(release)
	for i := 0; i < 6; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestBlogImageProcessorWaitingRequestHonorsContext(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	processor := newBlogImageProcessor(1, func([]byte, string) (blogimg.CompressResult, error) {
		entered <- struct{}{}
		<-release
		return blogimg.CompressResult{}, nil
	})
	go processor.Process(context.Background(), nil, "image/png")
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := processor.Process(ctx, nil, "image/png")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Process() error = %v, want context.Canceled", err)
	}
	close(release)
}
