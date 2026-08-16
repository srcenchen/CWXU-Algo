package opslock

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type Lock struct {
	file *os.File
	path string
}

func Acquire(path string, timeoutMS int) (*Lock, error) {
	if path == "" {
		path = "/run/lock/goalgo-ops.lock"
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建锁目录：%w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开锁文件：%w", err)
	}
	if timeoutMS <= 0 {
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			file.Close()
			return nil, fmt.Errorf("另一个操作正持有锁")
		}
	} else {
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
			file.Close()
			return nil, fmt.Errorf("获取锁：%w", err)
		}
	}
	if err := file.Truncate(0); err == nil {
		fmt.Fprintf(file, "%d\n", os.Getpid())
		file.Sync()
	}
	return &Lock{file: file, path: path}, nil
}

func (l *Lock) Release() {
	if l == nil || l.file == nil {
		return
	}
	syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	l.file.Close()
	l.file = nil
}
