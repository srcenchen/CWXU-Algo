package service

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"mime"
	"path"
	"strings"
	"unicode/utf8"
)

const (
	maxStaticZipBytes      = 32 << 20
	maxStaticUnpackedBytes = 64 << 20
	maxStaticFileBytes     = 8 << 20
	maxStaticFileCount     = 200
	maxStaticPathLen       = 180
)

// staticZipFile is one unpacked file ready to upload.
type staticZipFile struct {
	RelPath     string
	Data        []byte
	ContentType string
}

func unpackStaticZip(raw []byte) ([]staticZipFile, string) {
	if len(raw) == 0 {
		return nil, "请上传 zip 压缩包"
	}
	if len(raw) > maxStaticZipBytes {
		return nil, "压缩包过大，最大 32MB"
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, "压缩包无法打开，请确认是 zip 文件"
	}
	if len(zr.File) == 0 {
		return nil, "压缩包是空的"
	}
	if len(zr.File) > maxStaticFileCount {
		return nil, fmt.Sprintf("文件过多，最多 %d 个", maxStaticFileCount)
	}

	var total int64
	out := make([]staticZipFile, 0, len(zr.File))
	seen := make(map[string]struct{}, len(zr.File))
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rel, msg := cleanStaticRelPath(f.Name)
		if msg != "" {
			return nil, msg
		}
		if _, ok := seen[rel]; ok {
			return nil, "压缩包里有重复文件"
		}
		seen[rel] = struct{}{}
		if f.UncompressedSize64 > maxStaticFileBytes {
			return nil, "单个文件过大，最大 8MB"
		}
		rc, err := f.Open()
		if err != nil {
			return nil, "压缩包读取失败"
		}
		data, err := io.ReadAll(io.LimitReader(rc, maxStaticFileBytes+1))
		_ = rc.Close()
		if err != nil {
			return nil, "压缩包读取失败"
		}
		if int64(len(data)) > maxStaticFileBytes {
			return nil, "单个文件过大，最大 8MB"
		}
		total += int64(len(data))
		if total > maxStaticUnpackedBytes {
			return nil, "解压后总体积过大，最大 64MB"
		}
		if msg := rejectStaticFile(rel, data); msg != "" {
			return nil, msg
		}
		out = append(out, staticZipFile{
			RelPath:     rel,
			Data:        data,
			ContentType: staticContentType(rel),
		})
	}
	if len(out) == 0 {
		return nil, "压缩包里没有文件"
	}
	return out, ""
}

func cleanStaticRelPath(name string) (string, string) {
	name = strings.ReplaceAll(name, "\\", "/")
	name = strings.TrimSpace(name)
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\x00") {
		return "", "压缩包路径不合法"
	}
	clean := path.Clean(name)
	if clean == "." || strings.HasPrefix(clean, "../") || clean == ".." {
		return "", "压缩包路径不合法"
	}
	if utf8.RuneCountInString(clean) > maxStaticPathLen {
		return "", "文件路径过长"
	}
	for _, part := range strings.Split(clean, "/") {
		if part == "" || part == "." || part == ".." {
			return "", "压缩包路径不合法"
		}
	}
	return clean, ""
}

func rejectStaticFile(rel string, data []byte) string {
	ext := strings.ToLower(path.Ext(rel))
	switch ext {
	case ".html", ".htm":
		if looksLikeHTMLScriptBomb(data) {
			return "HTML 里不能包含服务端脚本"
		}
	case ".svg":
		if looksLikeSVG(data) && svgHasDanger(data) {
			return "SVG 不能包含脚本"
		}
	case ".exe", ".dll", ".so", ".dylib", ".bat", ".cmd", ".sh", ".php", ".jsp", ".asp", ".aspx":
		return "压缩包包含不允许的文件类型"
	}
	return ""
}

func looksLikeHTMLScriptBomb(data []byte) bool {
	head := bytes.ToLower(data)
	if len(head) > 4096 {
		head = head[:4096]
	}
	return bytes.Contains(head, []byte("<?php")) || bytes.Contains(head, []byte("<%"))
}

func svgHasDanger(data []byte) bool {
	lower := strings.ToLower(string(data))
	for _, bad := range svgDangerous {
		if strings.Contains(lower, bad) {
			return true
		}
	}
	return false
}

func staticContentType(rel string) string {
	ext := strings.ToLower(path.Ext(rel))
	switch ext {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".txt", ".md":
		return "text/plain; charset=utf-8"
	case ".wasm":
		return "application/wasm"
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

func staticZipHasEntry(files []staticZipFile, entry string) bool {
	entry = strings.TrimPrefix(strings.ReplaceAll(entry, "\\", "/"), "/")
	for _, f := range files {
		if f.RelPath == entry {
			return true
		}
	}
	return false
}

func defaultStaticEntry(files []staticZipFile) string {
	for _, name := range []string{"index.html", "index.htm"} {
		if staticZipHasEntry(files, name) {
			return name
		}
	}
	for _, f := range files {
		ext := strings.ToLower(path.Ext(f.RelPath))
		if ext == ".html" || ext == ".htm" {
			if !strings.Contains(f.RelPath, "/") {
				return f.RelPath
			}
		}
	}
	return ""
}
